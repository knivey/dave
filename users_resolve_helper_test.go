package main

import (
	"errors"
	"testing"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
)

type capturingReplier struct {
	msgs []string
}

func (c *capturingReplier) Reply(_ girc.Event, msg string) {
	c.msgs = append(c.msgs, msg)
}

func makeTestEvent(nick string) girc.Event {
	return girc.Event{
		Source: &girc.Source{Name: nick, Ident: "ident", Host: "host"},
	}
}

func TestHandleResolveResultHappyPath(t *testing.T) {
	// Notices must have defaults populated for templating.
	resetNoticesForTest(t)

	rep := &capturingReplier{}
	user := &User{ID: 42, Flagged: false}
	proceed, uid := handleResolveResultWithReplier(rep, makeTestEvent("Alice"), user, nil)
	assert.True(t, proceed)
	assert.Equal(t, int64(42), uid)
	assert.Empty(t, rep.msgs, "no notice should be sent on happy path")
}

func TestHandleResolveResultFlaggedRow(t *testing.T) {
	resetNoticesForTest(t)

	rep := &capturingReplier{}
	user := &User{ID: 99, Flagged: true, FlaggedReason: FlaggedReasonCollision}
	proceed, uid := handleResolveResultWithReplier(rep, makeTestEvent("Bob"), user, nil)
	assert.True(t, proceed, "flagged user should still proceed")
	assert.Equal(t, int64(99), uid)
	if assert.Len(t, rep.msgs, 1) {
		assert.Contains(t, rep.msgs[0], "Bob", "notice should mention the nick")
	}
}

func TestHandleResolveResultTransient(t *testing.T) {
	resetNoticesForTest(t)

	rep := &capturingReplier{}
	err := &ErrUserResolveTransient{Err: errors.New("database is locked")}
	proceed, uid := handleResolveResultWithReplier(rep, makeTestEvent("Carol"), nil, err)
	assert.False(t, proceed)
	assert.Equal(t, int64(0), uid)
	if assert.Len(t, rep.msgs, 1) {
		assert.Contains(t, rep.msgs[0], "try again")
		assert.Contains(t, rep.msgs[0], "Carol")
	}
}

func TestHandleResolveResultCollision(t *testing.T) {
	resetNoticesForTest(t)

	rep := &capturingReplier{}
	err := &ErrUserResolveCollision{
		Network: "net", Nick: "Dave", Account: "",
		Err: errors.New("UNIQUE constraint failed: synthetic"),
	}
	proceed, uid := handleResolveResultWithReplier(rep, makeTestEvent("Dave"), nil, err)
	assert.False(t, proceed)
	assert.Equal(t, int64(0), uid)
	if assert.Len(t, rep.msgs, 1) {
		assert.Contains(t, rep.msgs[0], "Dave")
	}
}

func TestHandleResolveResultUnknownErr(t *testing.T) {
	resetNoticesForTest(t)

	rep := &capturingReplier{}
	proceed, uid := handleResolveResultWithReplier(rep, makeTestEvent("Eve"), nil, errors.New("disk full"))
	assert.False(t, proceed)
	assert.Equal(t, int64(0), uid)
	assert.Empty(t, rep.msgs, "unknown errors should be silent (already logged upstream)")
}

func TestHandleResolveResultNilUser(t *testing.T) {
	resetNoticesForTest(t)

	rep := &capturingReplier{}
	proceed, uid := handleResolveResultWithReplier(rep, makeTestEvent("Frank"), nil, nil)
	assert.False(t, proceed)
	assert.Equal(t, int64(0), uid)
	assert.Empty(t, rep.msgs)
}

// resetNoticesForTest installs a fresh defaults-populated Notices into the
// shared config so getNotices() returns templated strings instead of empty.
func resetNoticesForTest(t *testing.T) {
	t.Helper()
	n := NoticesConfig{}
	setNoticesDefaults(&n)
	configMu.Lock()
	config.Notices = n
	configMu.Unlock()
}
