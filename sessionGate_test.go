package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionGateSerializesSameSession pins the core contract: a second
// acquire on the SAME session blocks until the holder releases.
func TestSessionGateSerializesSameSession(t *testing.T) {
	release1, ok := acquireSessionTurn(context.Background(), 101, sessionTurnWaitLimit)
	require.True(t, ok, "first acquire should succeed")

	acquired := make(chan struct{})
	go func() {
		release2, ok2 := acquireSessionTurn(context.Background(), 101, sessionTurnWaitLimit)
		assert.True(t, ok2, "second acquire should succeed after release")
		release2()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("second acquire completed while the gate was still held")
	case <-time.After(150 * time.Millisecond):
	}

	release1()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire did not complete after release")
	}
}

// TestSessionGateDifferentSessionsIndependent: holding one session's gate
// must never block another session (concurrency between users / fresh chat
// commands is the point of the design).
func TestSessionGateDifferentSessionsIndependent(t *testing.T) {
	release, ok := acquireSessionTurn(context.Background(), 201, sessionTurnWaitLimit)
	require.True(t, ok)
	defer release()

	done := make(chan struct{})
	go func() {
		release2, ok2 := acquireSessionTurn(context.Background(), 202, sessionTurnWaitLimit)
		assert.True(t, ok2)
		release2()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("different session was blocked by an unrelated held gate")
	}
}

// TestSessionGateWaiterAbortsOnCtxCancel: a waiter whose context is
// cancelled while waiting must return not-ok (never run its turn after the
// wait), and the gate must remain usable afterwards.
func TestSessionGateWaiterAbortsOnCtxCancel(t *testing.T) {
	release, ok := acquireSessionTurn(context.Background(), 301, sessionTurnWaitLimit)
	require.True(t, ok)

	ctx, cancel := context.WithCancel(context.Background())
	aborted := make(chan bool, 1)
	go func() {
		rel, ok2 := acquireSessionTurn(ctx, 301, sessionTurnWaitLimit)
		aborted <- !ok2
		if ok2 {
			rel()
		}
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case v := <-aborted:
		require.True(t, v, "cancelled waiter should report not-acquired")
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}

	release()

	rel2, ok3 := acquireSessionTurn(context.Background(), 301, sessionTurnWaitLimit)
	require.True(t, ok3, "gate unusable after cancelled waiter")
	rel2()
}

// TestSessionGateSequentialReuse: acquire/release cycles on the same
// session id keep working (release must fully reset the gate).
func TestSessionGateSequentialReuse(t *testing.T) {
	for i := 0; i < 3; i++ {
		release, ok := acquireSessionTurn(context.Background(), 401, sessionTurnWaitLimit)
		require.True(t, ok, "cycle %d should acquire", i)
		release()
	}
}

// TestSessionGateZeroSessionNoOp: ephemeral turns (session 0) never gate.
func TestSessionGateZeroSessionNoOp(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := acquireSessionTurn(context.Background(), 0, sessionTurnWaitLimit)
			assert.True(t, ok)
			release()
		}()
	}
	wg.Wait()
}

// TestSessionTurnBusy: the notice hook reports held/free accurately.
func TestSessionTurnBusy(t *testing.T) {
	require.False(t, sessionTurnBusy(501), "fresh session should not be busy")
	release, ok := acquireSessionTurn(context.Background(), 501, sessionTurnWaitLimit)
	require.True(t, ok)
	require.True(t, sessionTurnBusy(501), "held session should be busy")
	release()
	require.False(t, sessionTurnBusy(501), "released session should not be busy")
}

// TestNoticesQueueTurnWaitDefault: the waiting notice has a default.
func TestNoticesQueueTurnWaitDefault(t *testing.T) {
	var n NoticesConfig
	setNoticesDefaults(&n)
	assert.NotEmpty(t, n.Queue.TurnWait, "queue.turn_wait should have a default")
	assert.NotEmpty(t, n.Queue.TurnWaitTimeout, "queue.turn_wait_timeout should have a default")
}

// TestSessionGateWaitLimitAborts: the liveness valve. A waiter must not
// block forever — if the holder never releases (e.g. it is itself wedged
// on a full output buffer behind the waiter's delivery slot), the waiter
// aborts after sessionTurnWaitLimit WITHOUT needing ctx cancellation.
// Without the valve this test fails: the return only comes from the 5s
// test-guard ctx cancellation (elapsed too high, ctx cancelled).
func TestSessionGateWaitLimitAborts(t *testing.T) {
	release, ok := acquireSessionTurn(context.Background(), 601, sessionTurnWaitLimit)
	require.True(t, ok)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		_, ok2 := acquireSessionTurn(ctx, 601, 100*time.Millisecond)
		returned <- time.Since(start)
		if ok2 {
			t.Error("waiter should not have acquired the held gate")
		}
	}()

	select {
	case elapsed := <-returned:
		require.Less(t, elapsed, 2*time.Second, "waiter should abort via the wait limit, not the ctx guard")
		require.NoError(t, ctx.Err(), "ctx should not have been cancelled — the valve fired, not the guard")
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never returned: gate wait is unbounded (liveness valve missing)")
	}

	release()
}

// TestChatSessionGateWaitLimitNotice: when the valve fires on a chat()
// waiter, the user is told to resend, nothing is persisted, and no API
// call is made. The floor is shrunk to 40ms and cfg.Timeout set to 60ms,
// so the effective limit is the DERIVED 3×60ms=180ms (exercising
// sessionGateWaitLimitFor at the real call site — the derivation's exact
// math is pinned separately in TestSessionGateWaitLimitFor).
func TestChatSessionGateWaitLimitNotice(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	origLimit := sessionTurnWaitLimit
	sessionTurnWaitLimit = 40 * time.Millisecond
	t.Cleanup(func() { sessionTurnWaitLimit = origLimit })

	userID := ensureTestUser(t, "testnet", "valveuser")
	resolved := chatGateUser(userID, "valveuser")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no API call should happen when the waiter aborts on the wait limit")
		chatGateCompletion(w, "unused")
	}))
	t.Cleanup(func() { server.Close() })
	setChatGateServices(t, server.URL+"/v1")

	sid := createTestSession(t, "testnet", "#valve", "valveuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	release, ok := acquireSessionTurn(context.Background(), sid, sessionTurnWaitLimit)
	require.True(t, ok, "test should hold the gate")

	cfg := makeTestAIConfig()
	cfg.Timeout = 60 * time.Millisecond // derived limit: 3 × 60ms = 180ms

	// Test-hang guard only — the valve must fire long before this.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(5 * time.Second)
		cancel()
	}()

	out := make(chan string, 50)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		chat(Network{Name: "testnet"}, chatGateClient(), chatGateEvent("valveuser", "#valve"), cfg, ctx, out, resolved, "valve question")
	}()

	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("chat() never returned from the gate wait")
	}

	sawTimeout := false
	for l := range out {
		if strings.Contains(l, "still generating") {
			sawTimeout = true
		}
	}
	assert.True(t, sawTimeout, "waiter never received the wait-limit notice")

	msgs, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	for _, m := range msgs {
		assert.NotContains(t, m.Content, "valve question", "aborted waiter must not persist its user message")
	}

	release()
}

// TestSessionGateWaitLimitFor: the derivation table — the effective limit
// is max(floor, 3×timeout); the floor protects short/unset timeouts, the
// derived arm keeps the valve above long-timeout legitimate turns.
func TestSessionGateWaitLimitFor(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"zero timeout keeps floor", 0, sessionTurnWaitLimit},
		{"default 60s keeps floor", 60 * time.Second, sessionTurnWaitLimit},
		{"3m timeout stays under floor", 3 * time.Minute, sessionTurnWaitLimit},
		{"5m timeout derives 15m", 5 * time.Minute, 15 * time.Minute},
		{"15m timeout derives 45m", 15 * time.Minute, 45 * time.Minute},
		{"1h timeout derives 3h", time.Hour, 3 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sessionGateWaitLimitFor(tt.timeout))
		})
	}
}

// ---- wiring tests: chat() and deliverAsyncResult hold the session gate ----

// setChatGateServices points the "testsvc" service at baseURL while
// preserving the rest of the config (setupMockDeps installs Commands).
func setChatGateServices(t *testing.T, baseURL string) {
	t.Helper()
	configMu.Lock()
	old := config
	c := config
	c.Services = map[string]Service{"testsvc": {BaseURL: baseURL}}
	config = c
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		config = old
		configMu.Unlock()
	})
}

func chatGateEvent(nick, channel string) girc.Event {
	return girc.Event{
		Source: &girc.Source{Name: nick, Ident: "u", Host: "h"},
		Params: []string{channel},
	}
}

func chatGateUser(userID int64, nick string) *User {
	return &User{ID: userID, Network: "testnet", CurrentNick: nick}
}

func chatGateClient() *girc.Client {
	return girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})
}

// chatGateCompletion encodes a non-streaming chat completion response.
func chatGateCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "cmpl-1", "object": "chat.completion", "created": 1, "model": "test-model",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
	})
}

// TestChatSessionGateWaitsForInFlightTurn is the core wiring pin: a second
// continuation on a session with a turn in flight must (a) not reach the
// API, (b) not persist its user message, (c) receive the waiting notice,
// and after the holder finishes (d) run with the first turn's answer in
// its context, leaving causally-ordered rows. The gate lives in chat()
// before any streaming/non-streaming branch, so the non-streaming shape
// here covers the hold for every variant.
func TestChatSessionGateWaitsForInFlightTurn(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "gateuser")
	resolved := chatGateUser(userID, "gateuser")

	var mu sync.Mutex
	var bodies []string

	firstStarted := make(chan struct{})
	var firstOnce sync.Once
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(releaseFirst) }) }

	secondStarted := make(chan struct{})
	var secondOnce sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()

		if strings.Contains(body, "second question") {
			secondOnce.Do(func() { close(secondStarted) })
			chatGateCompletion(w, "second answer")
			return
		}
		firstOnce.Do(func() { close(firstStarted) })
		<-releaseFirst
		chatGateCompletion(w, "first answer")
	}))
	// Cleanup ordering is load-bearing: doRelease must run BEFORE
	// server.Close() (which waits for outstanding requests), so both are
	// t.Cleanups with the release registered last (LIFO runs it first).
	t.Cleanup(func() { server.Close() })
	t.Cleanup(doRelease)
	setChatGateServices(t, server.URL+"/v1")

	network := Network{Name: "testnet"}
	client := chatGateClient()
	cfg := makeTestAIConfig()

	outA := make(chan string, 50)
	outB := make(chan string, 50)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(outA)
		chat(network, client, chatGateEvent("gateuser", "#gate"), cfg, context.Background(), outA, resolved, "first question")
	}()

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never reached the API")
	}

	go func() {
		defer wg.Done()
		defer close(outB)
		chat(network, client, chatGateEvent("gateuser", "#gate"), cfg, context.Background(), outB, resolved, "second question")
	}()

	// The second turn must not reach the API while the first is in flight.
	select {
	case <-secondStarted:
		t.Fatal("second turn reached the API before the first turn completed")
	case <-time.After(250 * time.Millisecond):
	}

	// Nor may it persist its user message before acquiring the gate.
	sess, err := sessionMgr.GetActiveSession("testnet", "#gate", userID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	msgs, err := sessionMgr.GetMessages(sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		assert.NotContains(t, m.Content, "second question",
			"waiter persisted its user message before acquiring the session gate")
	}

	doRelease()
	wg.Wait()

	select {
	case <-secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("second turn never reached the API after the first turn released")
	}

	// Causal history order: system, userA, assistantA, userB, assistantB.
	msgs, err = sessionMgr.GetMessages(sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 5, "expected system + two full turns, got %s", summarizeMessages(msgs))
	assert.Equal(t, RoleSystem, msgs[0].Role)
	assert.Equal(t, RoleUser, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "first question")
	assert.Equal(t, RoleAssistant, msgs[2].Role)
	assert.Contains(t, msgs[2].Content, "first answer")
	assert.Equal(t, RoleUser, msgs[3].Role)
	assert.Contains(t, msgs[3].Content, "second question")
	assert.Equal(t, RoleAssistant, msgs[4].Role)
	assert.Contains(t, msgs[4].Content, "second answer")

	// The second request carried the first turn's answer in its context.
	mu.Lock()
	require.Len(t, bodies, 2)
	assert.Contains(t, bodies[1], "first answer",
		"second request did not include the first turn's answer")
	mu.Unlock()

	// The waiter was told it was waiting.
	sawWaitNotice := false
	var bLines []string
	for l := range outB {
		bLines = append(bLines, l)
	}
	for _, line := range bLines {
		if strings.Contains(line, "still working on your previous message") {
			sawWaitNotice = true
		}
	}
	assert.True(t, sawWaitNotice, "waiter never received the turn_wait notice")
}

// TestChatSessionGateDifferentUsersConcurrent pins the scope requirement:
// turns on DIFFERENT sessions (here: different users) must still execute
// concurrently — the gate must never degrade into a per-user or per-channel
// lock.
func TestChatSessionGateDifferentUsersConcurrent(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	ua := ensureTestUser(t, "testnet", "alicec")
	ub := ensureTestUser(t, "testnet", "bobc")

	arrived := make(chan struct{}, 2)
	releaseAll := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(releaseAll) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-releaseAll
		chatGateCompletion(w, "done")
	}))
	// Cleanup ordering is load-bearing: doRelease must run BEFORE
	// server.Close() (which waits for outstanding requests), so both are
	// t.Cleanups with the release registered last (LIFO runs it first).
	t.Cleanup(func() { server.Close() })
	t.Cleanup(doRelease)
	setChatGateServices(t, server.URL+"/v1")

	network := Network{Name: "testnet"}
	client := chatGateClient()
	cfg := makeTestAIConfig()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		out := make(chan string, 50)
		defer close(out)
		chat(network, client, chatGateEvent("alicec", "#conc"), cfg, context.Background(), out, chatGateUser(ua, "alicec"), "hello from alice")
	}()
	go func() {
		defer wg.Done()
		out := make(chan string, 50)
		defer close(out)
		chat(network, client, chatGateEvent("bobc", "#conc"), cfg, context.Background(), out, chatGateUser(ub, "bobc"), "hello from bob")
	}()

	// Both turns must be in flight simultaneously.
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("both users' turns never ran concurrently (gate over-serialized)")
		}
	}
	doRelease()
	wg.Wait()

	sa, err := sessionMgr.GetActiveSession("testnet", "#conc", ua)
	require.NoError(t, err)
	require.NotNil(t, sa)
	sb, err := sessionMgr.GetActiveSession("testnet", "#conc", ub)
	require.NoError(t, err)
	require.NotNil(t, sb)
	assert.NotEqual(t, sa.ID, sb.ID, "users should have distinct sessions")
}

// TestChatSessionGatePrefixCommandNotBlockedByInFlightTurn pins the other
// scope requirement: a fresh chat command runs immediately even while a
// continuation turn is still in flight, because the prefix dispatch
// (ClearContext + new session) lands on a different gate key.
func TestChatSessionGatePrefixCommandNotBlockedByInFlightTurn(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "prefixuser")
	resolved := chatGateUser(userID, "prefixuser")

	mentionStarted := make(chan struct{})
	var mentionOnce sync.Once
	releaseMention := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(releaseMention) }) }

	prefixStarted := make(chan struct{})
	var prefixOnce sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if strings.Contains(body, "prefix question") {
			prefixOnce.Do(func() { close(prefixStarted) })
			chatGateCompletion(w, "prefix answer")
			return
		}
		mentionOnce.Do(func() { close(mentionStarted) })
		<-releaseMention
		chatGateCompletion(w, "mention answer")
	}))
	// Cleanup ordering is load-bearing: doRelease must run BEFORE
	// server.Close() (which waits for outstanding requests), so both are
	// t.Cleanups with the release registered last (LIFO runs it first).
	t.Cleanup(func() { server.Close() })
	t.Cleanup(doRelease)
	setChatGateServices(t, server.URL+"/v1")

	network := Network{Name: "testnet"}
	client := chatGateClient()
	cfg := makeTestAIConfig()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		out := make(chan string, 50)
		defer close(out)
		chat(network, client, chatGateEvent("prefixuser", "#prefix"), cfg, context.Background(), out, resolved, "mention question")
	}()

	select {
	case <-mentionStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("mention turn never reached the API")
	}

	// Simulate the prefix-command dispatch sequence: ClearContext, then a
	// fresh chat() that must create its own session and run immediately.
	ClearContext("testnet", "#prefix", userID)
	go func() {
		defer wg.Done()
		out := make(chan string, 50)
		defer close(out)
		chat(network, client, chatGateEvent("prefixuser", "#prefix"), cfg, context.Background(), out, resolved, "prefix question")
	}()

	select {
	case <-prefixStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("prefix chat command was blocked by the in-flight mention turn")
	}

	doRelease()
	wg.Wait()

	active, err := sessionMgr.GetActiveSession("testnet", "#prefix", userID)
	require.NoError(t, err)
	require.NotNil(t, active, "expected the prefix command's new session to be active")
}

// TestChatSessionGateCancelledWaiterDoesNotPersistOrRun: a waiter whose
// context is cancelled while blocked must abort without persisting its
// user message or hitting the API, and must not disturb the holder.
func TestChatSessionGateCancelledWaiterDoesNotPersistOrRun(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "canceluser")
	resolved := chatGateUser(userID, "canceluser")

	var mu sync.Mutex
	var bodies []string

	firstStarted := make(chan struct{})
	var firstOnce sync.Once
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(releaseFirst) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		firstOnce.Do(func() { close(firstStarted) })
		<-releaseFirst
		chatGateCompletion(w, "first answer")
	}))
	// Cleanup ordering is load-bearing: doRelease must run BEFORE
	// server.Close() (which waits for outstanding requests), so both are
	// t.Cleanups with the release registered last (LIFO runs it first).
	t.Cleanup(func() { server.Close() })
	t.Cleanup(doRelease)
	setChatGateServices(t, server.URL+"/v1")

	network := Network{Name: "testnet"}
	client := chatGateClient()
	cfg := makeTestAIConfig()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out := make(chan string, 50)
		defer close(out)
		chat(network, client, chatGateEvent("canceluser", "#cancel"), cfg, context.Background(), out, resolved, "first question")
	}()

	<-firstStarted

	bCtx, cancel := context.WithCancel(context.Background())
	bDone := make(chan struct{})
	outB := make(chan string, 50)
	go func() {
		defer close(bDone)
		defer close(outB)
		chat(network, client, chatGateEvent("canceluser", "#cancel"), cfg, bCtx, outB, resolved, "second question")
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()
	<-bDone

	// Cancel path is SILENT beyond the initial turn_wait warning: the queue
	// already told the user (stop notice), and no resend-notice may fire —
	// the turn_wait_timeout notice is reserved for the wait-limit valve.
	var bLines []string
	for l := range outB {
		bLines = append(bLines, l)
	}
	sawWait, sawTimeout := false, false
	for _, line := range bLines {
		if strings.Contains(line, "still working on your previous message") {
			sawWait = true
		}
		if strings.Contains(line, "still generating") {
			sawTimeout = true
		}
	}
	assert.True(t, sawWait, "waiter should have received the turn_wait notice before blocking")
	assert.False(t, sawTimeout, "cancelled waiter must NOT receive the turn_wait_timeout notice")

	sess, err := sessionMgr.GetActiveSession("testnet", "#cancel", userID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	msgs, err := sessionMgr.GetMessages(sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		assert.NotContains(t, m.Content, "second question",
			"cancelled waiter persisted its user message")
	}
	mu.Lock()
	assert.Len(t, bodies, 1, "cancelled waiter hit the API")
	mu.Unlock()

	doRelease()
	wg.Wait()

	mu.Lock()
	assert.Len(t, bodies, 1, "extra API call after holder completed")
	mu.Unlock()

	msgs, err = sessionMgr.GetMessages(sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 3, "expected system + one full turn")
	assert.Contains(t, msgs[2].Content, "first answer")
}

// TestDeliverAsyncResultWaitsForInFlightTurn: async background-job delivery
// runs a turn on the session and must wait for an in-flight turn instead of
// interleaving rows with it. The holder here is a direct gate acquisition
// (that a real user turn holds exactly this gate is pinned by
// TestChatSessionGateWaitsForInFlightTurn): a real chat() holder would
// deliver the completed job through its own inline post-turn loop — the
// designed first-wins path — so the job would never reach this goroutine.
func TestDeliverAsyncResultWaitsForInFlightTurn(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")
	require.NoError(t, createPendingJob(sid, "job-1", "generate_image_async", "img-mcp"))
	require.NoError(t, completePendingJob("job-1", "image url: http://example.com/img.png"))

	turnCalled := make(chan struct{})
	var turnOnce sync.Once
	var received []ChatMessage
	var recvMu sync.Mutex
	origNewRunner := asyncJobMgr.newChatRunner
	asyncJobMgr.newChatRunner = func(network Network, client *girc.Client, c AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		return &mockChatRunner{
			runTurnFn: func(turn *turnContext) bool {
				recvMu.Lock()
				received = turn.Messages()
				recvMu.Unlock()
				turnOnce.Do(func() { close(turnCalled) })
				return true
			},
		}
	}
	defer func() { asyncJobMgr.newChatRunner = origNewRunner }()

	userID := ensureTestUser(t, "testnet", "testuser")
	entry := &jobEntry[asyncJobPayload]{
		jobID:     "job-1",
		payload:   asyncJobPayload{sessionID: sid},
		toolName:  "generate_image_async",
		mcpServer: "img-mcp",
		network:   "testnet",
		channel:   "#test",
		nick:      "testuser",
		userID:    userID,
	}

	release, ok := acquireSessionTurn(context.Background(), sid, sessionTurnWaitLimit)
	require.True(t, ok, "holder should acquire the session gate")

	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		deliverAsyncResult(entry, context.Background(), make(chan string, 50))
	}()

	select {
	case <-turnCalled:
		t.Fatal("async delivery ran its turn while the session gate was held")
	case <-time.After(250 * time.Millisecond):
	}

	release()

	select {
	case <-turnCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("async delivery never ran after the gate was released")
	}
	<-delivered

	// The delivery turn saw the injected result in its snapshot.
	recvMu.Lock()
	defer recvMu.Unlock()
	found := false
	for _, m := range received {
		if strings.Contains(m.Content, "Background task completed") {
			found = true
		}
	}
	assert.True(t, found, "delivery turn did not receive the injected async result")
}
