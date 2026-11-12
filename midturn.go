package main

import (
	"sync"
)

// Mid-turn message absorption (spec:
// docs/superpowers/specs/2026-11-12-midturn-injection-design.md).
//
// A same-session continuation (mention) arriving while its session's turn
// is running is handed to that live turn instead of queueing a second
// turn behind it: the running model is blind to queued input (it keeps
// executing a plan the user already superseded), and the queue
// choreography around a second turn (Queued → Processing → turn_wait)
// reads as the bot hallucinating.
//
// The mailbox is the hand-off point. dispatch (handleMention) pushes raw
// message text; the turn drains at round boundaries (executeToolCalls,
// after the view_image injection flush — the same wire shape view_image
// already proved in production) and in chat()'s post-turn quiescent-close
// loop. Race-freedom lives in one mutex: a push that succeeds while the
// mailbox is open is guaranteed to appear in a subsequent drain or in the
// closeAndDrain that flipped it closed — the only accepted-then-lost case
// is turn cancellation, where chat()'s deferred closeAndDrain persists
// the remainder as text rows.
//
// Registry entries are never deleted (closed mailboxes stay registered
// and reject pushes); the map is bounded by sessions since boot, exactly
// like the session-gate registry (sessionGate.go).
type turnMailbox struct {
	mu      sync.Mutex
	open    bool
	pending []string
}

var turnMailboxes sync.Map // int64 session id -> *turnMailbox

// openTurnMailbox opens a fresh mailbox for the session, closing any
// stale entry (a defensive replace — chat() holds the session gate, so
// two opens cannot legitimately race).
func openTurnMailbox(sessionID int64) *turnMailbox {
	mb := &turnMailbox{open: true}
	if old, loaded := turnMailboxes.Swap(sessionID, mb); loaded {
		omb := old.(*turnMailbox)
		omb.mu.Lock()
		omb.open = false
		omb.pending = nil
		omb.mu.Unlock()
	}
	return mb
}

// turnMailboxFor returns the session's mailbox, or nil when no turn has
// one open.
func turnMailboxFor(sessionID int64) *turnMailbox {
	mb, ok := turnMailboxes.Load(sessionID)
	if !ok {
		return nil
	}
	return mb.(*turnMailbox)
}

// push returns true when the turn has taken on the obligation to
// drain-or-persist the message.
func (mb *turnMailbox) push(msg string) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if !mb.open {
		return false
	}
	mb.pending = append(mb.pending, msg)
	return true
}

// drain returns and clears pending; the mailbox stays open.
func (mb *turnMailbox) drain() []string {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	out := mb.pending
	mb.pending = nil
	return out
}

// closeAndDrain atomically closes and returns pending: a racing push
// either lands in the returned slice or fails against the closed mailbox.
func (mb *turnMailbox) closeAndDrain() []string {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.open = false
	out := mb.pending
	mb.pending = nil
	return out
}

// tryAbsorbMention hands a continuation message to the session's live
// turn. Returns the receipt notice text and true when absorbed; ("", false)
// when no mailbox is open (dispatch falls back to the normal queue path).
func tryAbsorbMention(sessionID int64, msg, nick string) (string, bool) {
	mb := turnMailboxFor(sessionID)
	if mb == nil || !mb.push(msg) {
		return "", false
	}
	n := getNotices()
	return expandNotice(n.Mentions.Absorbed, map[string]string{"nick": nick}), true
}

// drainAbsorbedIntoTurn flushes absorbed messages into the turn at a
// round boundary: after all tool results and after view_image's
// pendingInjections, so the newest human input sits closest to the
// generation prompt. Row order: tool results → view_image injections →
// absorbed user messages (mirrors the flush order in executeToolCalls).
func drainAbsorbedIntoTurn(cr *chatRunner, turn *turnContext) {
	// Invariant: the mailbox lookup below keys on cr.sessionID while
	// persistence rides turn.Add under turn.sessionID — identical in
	// every production path (chat() and deliverAsyncResult set both to
	// the same session; ephemeral turns are 0-guarded here). A future
	// caller that lets them diverge must revisit this lookup.
	if cr.sessionID == 0 {
		return
	}
	mb := turnMailboxFor(cr.sessionID)
	if mb == nil {
		return
	}
	for _, raw := range mb.drain() {
		msg, fallback, ok := buildUserChatMessage(cr, raw, countContextImages(turn.Messages()))
		if !ok {
			// The warning already went out via the helper. The user was
			// told their message was absorbed — never drop it; keep the
			// text, skip the images.
			turn.Add(ChatMessage{Role: RoleUser, Content: fallback})
			continue
		}
		turn.Add(msg)
	}
}

// persistUndrainedAbsorbed is the loss-tail of the absorption obligation:
// every accepted push must be drained or persisted — exactly once, on
// every exit path. Two paths reach it with a non-empty mailbox: a
// cancelled turn (stop/shutdown — the post-turn loop bails on ctx.Done)
// and chat()'s early returns after the mailbox opened (image-budget
// drop, history-load failure). Either way the messages are written as
// plain text-only user rows so the user's words survive into their next
// turn; nothing is sent (the turn is over — cancelled turns were
// announced by the queue, early returns already sent their own error).
// On a normal exit the quiescent-close loop already closed the mailbox
// and this is a no-op. DB writes do not depend on the queue ctx.
func persistUndrainedAbsorbed(runner *chatRunner, mb *turnMailbox) {
	remaining := mb.closeAndDrain()
	if len(remaining) == 0 {
		return
	}
	persisted := 0
	for _, raw := range remaining {
		if err := sessionMgr.AddMessage(runner.sessionID, ChatMessage{Role: RoleUser, Content: raw}); err != nil {
			runner.logger.Error("mid-turn absorption: persist failed",
				"session", runner.sessionID, "error", err)
			continue
		}
		persisted++
	}
	runner.logger.Warn("mid-turn absorption: persisted undrained messages after turn end",
		"session", runner.sessionID, "count", persisted, "failed", len(remaining)-persisted)
}
