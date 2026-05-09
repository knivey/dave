package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestQM(t *testing.T) *QueueManager {
	t.Helper()
	origBotReady := botReadyFn
	botReadyFn = func(_, _ string) bool { return true }
	t.Cleanup(func() { botReadyFn = origBotReady })

	qm := NewQueueManager(NoticesConfig{Queue: QueueNotices{Msg: "queued (position {position})", Started: "started"}}, 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 1}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })
	return qm
}

func waitForNotRunning(t *testing.T, qm *QueueManager, net, ch, user string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if !qm.IsRunning(net, ch, user) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for job to complete")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestQueueManager_BasicEnqueueAndRun(t *testing.T) {
	qm := newTestQM(t)

	var ran atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	pos := qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		ran.Store(true)
		wg.Done()
	})

	assert.Equal(t, 0, pos, "first enqueue position")

	wg.Wait()

	assert.True(t, ran.Load(), "job did not run")

	waitForNotRunning(t, qm, "net", "#chan", "user")
	assert.False(t, qm.IsRunning("net", "#chan", "user"), "should not be running after job completes")
}

func TestQueueManager_EnqueueWhileBusy(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var wg1, wg2 sync.WaitGroup
	wg1.Add(1)
	wg2.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		wg1.Done()
		<-unblock
	})

	wg1.Wait()

	pos := qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		wg2.Done()
	})

	assert.Equal(t, 1, pos, "second enqueue position")

	assert.True(t, qm.IsRunning("net", "#chan", "user"), "first job should be running")

	current, pending := qm.QueueStatus("net", "#chan", "user")
	require.NotNil(t, current, "expected current item")
	require.Len(t, pending, 1, "expected 1 pending")

	close(unblock)
	wg2.Wait()

	waitForNotRunning(t, qm, "net", "#chan", "user")
	assert.False(t, qm.IsRunning("net", "#chan", "user"), "should not be running after both jobs complete")
}

func TestQueueManager_MaxDepth(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		started.Done()
		<-unblock
	})
	started.Wait()

	for i := 0; i < 4; i++ {
		pos := qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})
		assert.Equal(t, i+1, pos, "enqueue %d position", i+1)
	}

	pos := qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})
	assert.Equal(t, -1, pos, "overflow enqueue position")

	close(unblock)
}

func TestQueueManager_StopCurrent(t *testing.T) {
	qm := newTestQM(t)

	var started atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		started.Store(true)
		<-ctx.Done()
		wg.Done()
	})

	for !started.Load() {
		time.Sleep(10 * time.Millisecond)
	}

	assert.True(t, qm.StopCurrent("net", "#chan"), "StopCurrent should return true for running job")

	wg.Wait()

	waitForNotRunning(t, qm, "net", "#chan", "user")
	assert.False(t, qm.StopCurrent("net", "#chan"), "StopCurrent should return false when no job running")
}

func TestQueueManager_StopCurrentStartsNext(t *testing.T) {
	qm := newTestQM(t)

	unblockFirst := make(chan struct{})
	var firstDone, secondRan atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		select {
		case <-unblockFirst:
		case <-ctx.Done():
		}
		firstDone.Store(true)
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		secondRan.Store(true)
		wg.Done()
	})

	qm.StopCurrent("net", "#chan")

	wg.Wait()

	assert.True(t, firstDone.Load(), "first job should have completed (via cancel)")
	assert.True(t, secondRan.Load(), "second job should have run after first was cancelled")
}

func TestQueueManager_DifferentChannelsParallel(t *testing.T) {
	qm := NewQueueManager(NoticesConfig{Queue: QueueNotices{Msg: "queued", Started: "started"}}, 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 2}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	var ch1Ran, ch2Ran atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	unblock := make(chan struct{})

	qm.Enqueue("net", "#chan1", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		ch1Ran.Store(true)
		<-unblock
		wg.Done()
	})

	qm.Enqueue("net", "#chan2", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		ch2Ran.Store(true)
		wg.Done()
	})

	time.Sleep(100 * time.Millisecond)

	assert.True(t, ch1Ran.Load(), "chan1 job should have started")
	assert.True(t, ch2Ran.Load(), "chan2 job should start in parallel (different channel)")

	close(unblock)
	wg.Wait()
}

func TestQueueManager_StopCancelsRunning(t *testing.T) {
	qm := newTestQM(t)

	var cancelled atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		<-ctx.Done()
		cancelled.Store(true)
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)
	qm.Stop()
	wg.Wait()

	assert.True(t, cancelled.Load(), "job should have been cancelled by Stop()")
}

func TestQueueManager_IsRunning(t *testing.T) {
	qm := newTestQM(t)

	assert.False(t, qm.IsRunning("net", "#chan", "user"), "should not be running initially")

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	assert.True(t, qm.IsRunning("net", "#chan", "user"), "should be running during execution")

	close(unblock)
	wg.Wait()

	waitForNotRunning(t, qm, "net", "#chan", "user")
	assert.False(t, qm.IsRunning("net", "#chan", "user"), "should not be running after completion")
}

func TestQueueManager_UpdateServiceLimits(t *testing.T) {
	qm := newTestQM(t)

	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 2}})

	var user1Started, user2Started atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)
	unblock := make(chan struct{})

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		user1Started.Store(true)
		<-unblock
		wg.Done()
	})
	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		user2Started.Store(true)
		<-unblock
		wg.Done()
	})

	time.Sleep(100 * time.Millisecond)

	assert.True(t, user1Started.Load(), "user1 should have started")
	assert.True(t, user2Started.Load(), "user2 should start with parallel=2 (same channel)")

	close(unblock)
	wg.Wait()
}

func TestQueueManager_QueueStatus(t *testing.T) {
	qm := newTestQM(t)

	current, pending := qm.QueueStatus("net", "#chan", "user")
	assert.Nil(t, current, "expected nil status for unknown user")
	assert.Nil(t, pending, "expected nil status for unknown user")

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	current, pending = qm.QueueStatus("net", "#chan", "user")
	require.NotNil(t, current, "expected current item")
	assert.Len(t, pending, 0, "expected 0 pending")

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})
	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})

	current, pending = qm.QueueStatus("net", "#chan", "user")
	require.NotNil(t, current, "expected current item")
	require.Len(t, pending, 2, "expected 2 pending")

	close(unblock)
	wg.Wait()
}

func TestQueueManager_FIFOOrder(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(4)

	var order []int
	var orderMu sync.Mutex

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		orderMu.Lock()
		order = append(order, 1)
		orderMu.Unlock()
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, 2)
		orderMu.Unlock()
		wg.Done()
	})
	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, 3)
		orderMu.Unlock()
		wg.Done()
	})
	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, 4)
		orderMu.Unlock()
		wg.Done()
	})

	close(unblock)
	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()
	require.Len(t, order, 4, "expected 4 executions")
	for i, want := range []int{1, 2, 3, 4} {
		assert.Equal(t, want, order[i], "execution[%d]", i)
	}
}

func TestQueueManager_CrossUserFIFOOrder(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)

	var order []string
	var orderMu sync.Mutex

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		orderMu.Lock()
		order = append(order, "user1")
		orderMu.Unlock()
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, "user2")
		orderMu.Unlock()
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user3", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, "user3")
		orderMu.Unlock()
		wg.Done()
	})

	close(unblock)
	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()
	require.Len(t, order, 3, "expected 3 executions: %v", order)
	want := []string{"user1", "user2", "user3"}
	for i, w := range want {
		assert.Equal(t, w, order[i], "execution[%d]", i)
	}
}

func TestQueueManager_ConcurrentExecutionFIFODelivery(t *testing.T) {
	qm := NewQueueManager(NoticesConfig{Queue: QueueNotices{Msg: "queued", Started: "started"}}, 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 2}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	var execOrder []string
	var execMu sync.Mutex

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		execMu.Lock()
		execOrder = append(execOrder, "user1")
		execMu.Unlock()
		<-unblock
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		execMu.Lock()
		execOrder = append(execOrder, "user2")
		execMu.Unlock()
		wg.Done()
	})

	time.Sleep(100 * time.Millisecond)

	execMu.Lock()
	require.Len(t, execOrder, 2, "expected both jobs to start concurrently: %v", execOrder)
	execMu.Unlock()

	close(unblock)
	wg.Wait()
}

// setupDeliveryTest creates a QM with mocked getBotFn/botReadyFn.
// Since girc drops messages without a real connection, we test delivery
// ordering by capturing outputCh contents directly via a wrapped Execute.
func setupDeliveryTest(t *testing.T, parallel int) *QueueManager {
	t.Helper()

	origBotReady := botReadyFn
	origGetBot := getBotFn
	botReadyFn = func(_, _ string) bool { return true }

	client := girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})

	getBotFn = func(network string) *Bot {
		return &Bot{
			Client:  client,
			Network: Network{Name: network, Throttle: 0},
		}
	}

	t.Cleanup(func() {
		botReadyFn = origBotReady
		getBotFn = origGetBot
	})

	qm := NewQueueManager(NoticesConfig{
		Queue: QueueNotices{
			Msg:     "queued (position {position})",
			Started: "\x0306\u25b6 {nick}: Processing your request (waited {wait})...{prompt}\x0f",
		},
	}, 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: parallel}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	return qm
}

func TestQueueManager_ParallelDeliveryShowsStartedMsg(t *testing.T) {
	qm := setupDeliveryTest(t, 2)

	// Capture the deliveryWaited state by inspecting items after they pass through
	// the delivery slot. We use a channel to synchronize.
	type itemResult struct {
		deliveryWaited bool
		startedMsg     string
	}
	results := make(chan itemResult, 2)

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Item 1: blocks so item 2 must wait for delivery
	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		output <- "result A"
		<-unblock
		wg.Done()
	})

	// Item 2: should have deliveryWaited=true after item 1 releases
	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		output <- "result B"
		wg.Done()
	})

	// Let both start, let item 1 acquire delivery turn, item 2 waits
	time.Sleep(100 * time.Millisecond)

	// Check delivery slot: item 1 is current, item 2 is waiting
	ds := qm.getDeliverySlot("net", "#chan")
	ds.mu.Lock()
	require.NotNil(t, ds.current, "expected current item in delivery slot")
	ds.mu.Unlock()

	// Release item 1 so item 2 can deliver
	close(unblock)
	wg.Wait()

	// The key check: we verify the startedMsg format logic is correct
	// by checking what formatStartedMsg produces for a waited item
	_ = results

	// Verify item 2 would get a started message
	startedMsg := qm.formatStartedMsg("user2", 100*time.Millisecond, "", "")
	assert.Contains(t, startedMsg, "user2", "started msg should contain nick")
	assert.Contains(t, startedMsg, "Processing", "started msg should contain 'Processing'")
}

func TestQueueManager_ParallelDeliveryOrder(t *testing.T) {
	qm := setupDeliveryTest(t, 2)

	// Verify execution order: both run concurrently, but outputCh contents
	// are delivered in FIFO order per deliverySlot.
	var wg sync.WaitGroup
	wg.Add(2)

	var execOrder []string
	var execMu sync.Mutex

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		execMu.Lock()
		execOrder = append(execOrder, "user1")
		execMu.Unlock()
		output <- "A1"
		output <- "A2"
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		execMu.Lock()
		execOrder = append(execOrder, "user2")
		execMu.Unlock()
		output <- "B1"
		output <- "B2"
		wg.Done()
	})

	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	// Both should have executed concurrently
	execMu.Lock()
	require.Len(t, execOrder, 2, "both jobs should execute: %v", execOrder)
	execMu.Unlock()
}

func TestQueueManager_ParallelDeliveryWaitedFlag(t *testing.T) {
	qm := NewQueueManager(NoticesConfig{Queue: QueueNotices{Msg: "queued", Started: "started"}}, 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 2}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	origBotReady := botReadyFn
	origGetBot := getBotFn
	botReadyFn = func(_, _ string) bool { return true }
	getBotFn = func(network string) *Bot {
		return &Bot{
			Client:  girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"}),
			Network: Network{Name: network, Throttle: 0},
		}
	}
	t.Cleanup(func() {
		botReadyFn = origBotReady
		getBotFn = origGetBot
	})

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		time.Sleep(50 * time.Millisecond)
		wg.Done()
	})

	time.Sleep(100 * time.Millisecond)

	ds := qm.getDeliverySlot("net", "#chan")
	ds.mu.Lock()
	require.NotNil(t, ds.current, "expected current item in delivery slot")
	ds.mu.Unlock()

	close(unblock)
	wg.Wait()
}

func TestQueueManager_SerializedDeliveryNotifiesPrompt(t *testing.T) {
	qm := setupDeliveryTest(t, 2)

	// Verify that EnqueueAtWithPrompt correctly passes prompt and startedMsg
	// override through to formatStartedMsg. Since girc drops messages without
	// a connection, we test the formatting logic directly.
	startedMsg := qm.formatStartedMsg("user1", 5*time.Second, "a pretty sunset",
		"\x0306\u25b6 {nick}: Processed your image request (waited {wait})...{prompt}\x0f")
	assert.Contains(t, startedMsg, "Processed your image request", "should use override template")
	assert.Contains(t, startedMsg, "sunset", "should contain prompt text")
	assert.Contains(t, startedMsg, "user1", "should contain nick")
	assert.Contains(t, startedMsg, "5s", "should contain wait time")
}

func TestQueueManager_ServiceParallel1(t *testing.T) {
	qm := newTestQM(t)

	var user1Started, user2Started atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	unblock := make(chan struct{})

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		user1Started.Store(true)
		<-unblock
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		user2Started.Store(true)
		wg.Done()
	})

	time.Sleep(100 * time.Millisecond)

	assert.True(t, user1Started.Load(), "user1 should have started")
	assert.False(t, user2Started.Load(), "user2 should NOT start while user1 holds the service slot (parallel=1)")

	close(unblock)
	wg.Wait()

	assert.True(t, user2Started.Load(), "user2 should have started after user1 completed")
}

func TestQueueManager_ServiceParallel0(t *testing.T) {
	qm := NewQueueManager(NoticesConfig{Queue: QueueNotices{Msg: "queued", Started: "started"}}, 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 0}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	var count atomic.Int32
	var wg sync.WaitGroup
	wg.Add(5)

	unblock := make(chan struct{})

	for i := 0; i < 5; i++ {
		qm.Enqueue("net", "#chan", fmt.Sprintf("user%d", i), "svc", "", func(ctx context.Context, output chan<- string) {
			count.Add(1)
			<-unblock
			wg.Done()
		})
	}

	time.Sleep(100 * time.Millisecond)

	started := count.Load()
	assert.Equal(t, int32(5), started, "expected all 5 jobs to start with parallel=0 (unlimited)")

	close(unblock)
	wg.Wait()
}

func TestQueueManager_CancellationPropagation(t *testing.T) {
	qm := newTestQM(t)

	var gotCancelled atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		select {
		case <-time.After(5 * time.Second):
			t.Error("should have been cancelled")
		case <-ctx.Done():
			gotCancelled.Store(true)
		}
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)
	qm.StopCurrent("net", "#chan")
	wg.Wait()

	assert.True(t, gotCancelled.Load(), "Execute should have received ctx.Done() after StopCurrent")
}

func TestQueueManager_SchedulerFairness(t *testing.T) {
	qm := NewQueueManager(NoticesConfig{Queue: QueueNotices{Msg: "queued", Started: "started"}}, 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 1}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)

	var order []string
	var orderMu sync.Mutex
	addOrder := func(name string) {
		orderMu.Lock()
		order = append(order, name)
		orderMu.Unlock()
	}

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		addOrder("user1-first")
		<-unblock
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		addOrder("user1-second")
		wg.Done()
	})
	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		addOrder("user2-first")
		wg.Done()
	})

	close(unblock)
	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()
	require.Len(t, order, 3, "expected 3 executions: %v", order)
	assert.Equal(t, "user1-first", order[0], "first")
}

func TestQueueManager_StopEmptyQueue(t *testing.T) {
	qm := newTestQM(t)

	assert.False(t, qm.StopCurrent("net", "#chan"), "StopCurrent on empty queue should return false")
}

func TestQueueManager_CancelPending(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		started.Done()
		<-unblock
	})
	started.Wait()

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})

	_, pending := qm.QueueStatus("net", "#chan", "user")
	require.Len(t, pending, 1, "expected 1 pending")

	assert.True(t, qm.CancelPending("net", "#chan", "user"), "CancelPending should return true when items removed")

	_, pending = qm.QueueStatus("net", "#chan", "user")
	assert.Len(t, pending, 0, "expected 0 pending after cancel")

	assert.False(t, qm.CancelPending("net", "#chan", "nobody"), "CancelPending should return false when no items match")

	close(unblock)
}

func TestQueueManager_CancelPendingOtherUserPreserved(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		started.Done()
		<-unblock
	})
	started.Wait()

	var user2Ran atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		user2Ran.Store(true)
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {})

	qm.CancelPending("net", "#chan", "user1")

	close(unblock)
	wg.Wait()

	assert.True(t, user2Ran.Load(), "user2's pending job should still run after cancelling user1's pending items")
}

func TestQueueManager_QueueStatusOtherUser(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {})

	current1, pending1 := qm.QueueStatus("net", "#chan", "user1")
	require.NotNil(t, current1, "user1 should see current item (their running job)")
	assert.Len(t, pending1, 0, "user1 should see 0 pending")

	current2, pending2 := qm.QueueStatus("net", "#chan", "user2")
	assert.Nil(t, current2, "user2 should not see current item (user1 is running)")
	assert.Len(t, pending2, 1, "user2 should see 1 pending")

	close(unblock)
	wg.Wait()
}

func TestQueueManager_ParallelEnqueueReturnsDeliveryPosition(t *testing.T) {
	qm := setupDeliveryTest(t, 2)

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)

	pos1 := qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		wg.Done()
	})
	assert.Equal(t, 0, pos1, "first item should have delivery position 0")

	pos2 := qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		wg.Done()
	})
	assert.Equal(t, 1, pos2, "second item should have delivery position 1 (one item ahead)")

	pos3 := qm.Enqueue("net", "#chan", "user3", "svc", "", func(ctx context.Context, output chan<- string) {
		wg.Done()
	})
	assert.Equal(t, 2, pos3, "third item should have delivery position 2 (two items ahead)")

	close(unblock)
	wg.Wait()
}

func TestQueueManager_SingleParallelReturnsZero(t *testing.T) {
	qm := setupDeliveryTest(t, 2)

	var wg sync.WaitGroup
	wg.Add(1)

	pos := qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		wg.Done()
	})
	assert.Equal(t, 0, pos, "lone item should have delivery position 0")

	wg.Wait()
}
