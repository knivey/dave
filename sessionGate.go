package main

import (
	"context"
	"sync"
	"time"
)

var loggerSG = newLogger("sessionGate")

// DESIGN NOTE — why this exists: with a service's `parallel` > 1 (or a
// cross-service mix), the queue can run two completions for the same user
// at the same time, and the continuation (mention) path dispatches to
// whatever session is active — so two turns can execute on ONE session
// concurrently. Chat Completions turns snapshot history once at turn start
// (chat() → GetMessages → turnContext) and append rows as they complete,
// so the second turn's prompt misses the first turn's exchange, and the
// stored rows land causally scrambled (userB before assistantA, tool
// call/result pairs split by the sibling turn's rows). The Responses API
// path already serializes per user-context via responseChainMu; this gate
// gives the Chat Completions paths the same guarantee, scoped to the
// SESSION so that unrelated concurrency is untouched: two fresh chat
// commands (different sessions), a mention plus a new chat command
// (ClearContext completes the old session first), and different users
// (sessions are per network+channel+user) all still run in parallel.
//
// Waiters block BEFORE their user message is persisted (chat() acquires
// before AddMessage), so a cancelled waiter leaves no orphan row and the
// winner's snapshot always contains the previous turn's completed rows.
//
// Gate entries are never deleted from the map: each is a few dozen bytes,
// bounded by the number of sessions since boot, and deleting on last
// release would race with concurrent LoadOrStore for no measurable gain.
var sessionTurnGates sync.Map // map[int64]*sessionTurnGate

// sessionTurnGate is a handoff lock: the holder closes `ready` on release,
// waking all waiters; they re-contend for the gate under mu. Wait order is
// not FIFO — any waiter may win — which is acceptable: queue delivery
// ordering is the queue's job, this gate only guarantees exclusion.
type sessionTurnGate struct {
	mu    sync.Mutex
	held  bool
	ready chan struct{} // non-nil iff held; closed on release
}

func loadSessionTurnGate(sessionID int64) *sessionTurnGate {
	v, _ := sessionTurnGates.LoadOrStore(sessionID, &sessionTurnGate{})
	return v.(*sessionTurnGate)
}

// acquireSessionTurn blocks until the session's turn gate is free, the
// caller's context is cancelled, or sessionID <= 0 (ephemeral turns never
// gate). On success it returns the release func; the caller MUST call it
// (typically via defer) — release is idempotent and panic-safe through
// normal deferral. On cancellation it returns ok=false and the caller must
// abort the turn without persisting anything.
func acquireSessionTurn(ctx context.Context, sessionID int64) (release func(), ok bool) {
	if sessionID <= 0 {
		return func() {}, true
	}
	if ctx.Err() != nil {
		return nil, false
	}
	g := loadSessionTurnGate(sessionID)
	start := time.Now()
	waited := false
	for {
		g.mu.Lock()
		if !g.held {
			g.held = true
			g.ready = make(chan struct{})
			g.mu.Unlock()
			if waited {
				loggerSG.Debug("session turn gate acquired after wait",
					"session", sessionID,
					"wait", time.Since(start).Round(time.Millisecond))
			}
			return g.release, true
		}
		ready := g.ready
		g.mu.Unlock()
		waited = true
		select {
		case <-ctx.Done():
			return nil, false
		case <-ready:
			// select picks randomly among ready cases: check the
			// context again so a cancelled waiter never proceeds.
			if ctx.Err() != nil {
				return nil, false
			}
		}
	}
}

func (g *sessionTurnGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.held {
		return
	}
	g.held = false
	close(g.ready)
	g.ready = nil
}

// sessionTurnBusy reports whether a turn currently holds the session's
// gate. Advisory only — used to decide whether to send the waiting notice
// before blocking; racing with acquire/release at worst sends (or skips)
// one notice.
func sessionTurnBusy(sessionID int64) bool {
	if sessionID <= 0 {
		return false
	}
	v, ok := sessionTurnGates.Load(sessionID)
	if !ok {
		return false
	}
	g := v.(*sessionTurnGate)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}
