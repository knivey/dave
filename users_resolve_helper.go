package main

import (
	"errors"

	"github.com/lrstanley/girc"
)

// resolveResultReplier is the minimal subset of *girc.Client needed by
// handleResolveResult. Extracted as an interface so unit tests can capture
// outgoing replies without spinning up a real IRC client.
type resolveResultReplier interface {
	Reply(event girc.Event, msg string)
}

// gircClientReplier adapts *girc.Client to resolveResultReplier.
type gircClientReplier struct{ c *girc.Client }

func (g gircClientReplier) Reply(event girc.Event, msg string) {
	g.c.Cmd.Reply(event, msg)
}

// handleResolveResult inspects the outcome of resolveUser and emits the
// appropriate user-facing notice. Returns (proceed, userID).
//
//   - proceed=true: caller should continue processing with userID.
//   - proceed=false: the message has been handled (notice sent or silently
//     dropped); caller should return without further work.
//
// Branches:
//   - err is *ErrUserResolveTransient   -> send transient warn notice, stop.
//   - err is *ErrUserResolveCollision   -> send persistent warn notice, stop.
//   - err non-nil otherwise             -> silent drop (existing behavior).
//   - user==nil                          -> silent drop.
//   - user.Flagged                      -> send persistent warn notice, continue.
//   - normal user                       -> continue silently.
//
// The persistent notice is sent every time a flagged user is encountered;
// there is no per-user cooldown by design — admins want every flagged
// interaction surfaced.
func handleResolveResult(client *girc.Client, event girc.Event, user *User, err error) (bool, int64) {
	return handleResolveResultWithReplier(gircClientReplier{c: client}, event, user, err)
}

// handleResolveResultWithReplier is the testable form of handleResolveResult.
// Production code calls handleResolveResult which adapts *girc.Client.
func handleResolveResultWithReplier(rep resolveResultReplier, event girc.Event, user *User, err error) (bool, int64) {
	if err != nil {
		var transient *ErrUserResolveTransient
		var collision *ErrUserResolveCollision
		n := getNotices()
		vars := map[string]string{"nick": event.Source.Name}
		switch {
		case errors.As(err, &transient):
			rep.Reply(event, warnMsg(expandNotice(n.Users.ResolveTransient, vars)))
		case errors.As(err, &collision):
			rep.Reply(event, warnMsg(expandNotice(n.Users.ResolvePersistent, vars)))
		default:
			// Unknown DB error — silent drop; resolveUser caller already
			// logged the underlying error. Do not spam the channel with
			// generic "DB broken" messages from inside the hot path.
		}
		return false, 0
	}
	if user == nil {
		return false, 0
	}
	if user.Flagged {
		n := getNotices()
		rep.Reply(event, warnMsg(expandNotice(n.Users.ResolvePersistent,
			map[string]string{"nick": event.Source.Name})))
	}
	return true, user.ID
}
