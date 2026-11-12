package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddMessageAttributesOwner: the AddMessage chokepoint stamps
// user-role rows with the session owner's user id (per-message
// attribution); assistant rows stay unattributed.
func TestAddMessageAttributesOwner(t *testing.T) {
	setupTestDB(t)

	uid := ensureTestUser(t, "net", "speaker")
	sid, err := sessionMgr.CreateSession("net", "#c", uid, "cmd", "svc", "m")
	require.NoError(t, err)

	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "mine"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "dave's"}))

	msgs, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.NotNil(t, msgs[0].UserID, "user row is attributed")
	assert.Equal(t, uid, *msgs[0].UserID)
	assert.Nil(t, msgs[1].UserID, "assistant rows carry no author")
}

// TestCloneCopiesAuthorshipVerbatim: a clone's inherited user rows keep
// the SOURCE author's id (copied verbatim, never re-stamped), while
// messages the cloner sends afterwards attribute to the cloner.
func TestCloneCopiesAuthorshipVerbatim(t *testing.T) {
	setupTestDB(t)

	alice := ensureTestUser(t, "net", "alice")
	bob := ensureTestUser(t, "net", "bob")
	srcSid, err := sessionMgr.CreateSession("net", "#c", alice, "cmd", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "alice asks"}))

	cloneSid, err := cloneDBSession(srcSid, "net", "#c", bob, "sys", "alice")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(cloneSid, ChatMessage{Role: RoleUser, Content: "bob replies"}))

	msgs, err := sessionMgr.GetMessages(cloneSid)
	require.NoError(t, err)
	var aliceRows, bobRows int
	for _, m := range msgs {
		if m.Role != RoleUser {
			continue
		}
		switch m.Content {
		case "alice asks":
			require.NotNil(t, m.UserID, "inherited row keeps its author")
			assert.Equal(t, alice, *m.UserID, "alice's words stay alice's in bob's clone")
			aliceRows++
		case "bob replies":
			require.NotNil(t, m.UserID)
			assert.Equal(t, bob, *m.UserID, "post-clone rows attribute to the cloner")
			bobRows++
		}
	}
	assert.Equal(t, 1, aliceRows)
	assert.Equal(t, 1, bobRows)
}

// TestCloneOfCloneAuthorshipChain: A → clone(B) → clone(C): C's
// transcript holds A's, B's, and C's words each under the right id —
// attribution travels verbatim through any chain depth.
func TestCloneOfCloneAuthorshipChain(t *testing.T) {
	setupTestDB(t)

	alice := ensureTestUser(t, "net", "alice")
	bob := ensureTestUser(t, "net", "bob")
	carol := ensureTestUser(t, "net", "carol")

	aSid, err := sessionMgr.CreateSession("net", "#c", alice, "cmd", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(aSid, ChatMessage{Role: RoleUser, Content: "A1"}))

	bSid, err := cloneDBSession(aSid, "net", "#c", bob, "", "alice")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(bSid, ChatMessage{Role: RoleUser, Content: "B1"}))

	cSid, err := cloneDBSession(bSid, "net", "#c", carol, "", "bob")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(cSid, ChatMessage{Role: RoleUser, Content: "C1"}))

	msgs, err := sessionMgr.GetMessages(cSid)
	require.NoError(t, err)
	got := map[string]*int64{}
	for _, m := range msgs {
		if m.Role == RoleUser {
			got[m.Content] = m.UserID
		}
	}
	require.NotNil(t, got["A1"])
	assert.Equal(t, alice, *got["A1"], "A's words stay A's through two clone hops")
	require.NotNil(t, got["B1"])
	assert.Equal(t, bob, *got["B1"])
	require.NotNil(t, got["C1"])
	assert.Equal(t, carol, *got["C1"])
}

// legacyRows simulates pre-attribution data: NULL every message's
// user_id (the chokepoint stamps new writes, so tests must strip).
func legacyRows(t *testing.T, sid int64) {
	t.Helper()
	require.NoError(t, theDB.Exec("UPDATE messages SET user_id = NULL WHERE session_id = ?", sid).Error)
}

// TestBackfillMessageUserIDs exercises migration #11 on legacy rows:
// plain sessions backfill exactly; cloned sessions split inherited
// (aligned against the source's per-message attribution) from
// post-clone (cloner); clone-of-clone chains resolve transitively;
// the second run is a no-op.
func TestBackfillMessageUserIDs(t *testing.T) {
	setupTestDB(t)

	alice := ensureTestUser(t, "net", "alice")
	bob := ensureTestUser(t, "net", "bob")
	carol := ensureTestUser(t, "net", "carol")

	// Plain session.
	plainSid, err := sessionMgr.CreateSession("net", "#c", alice, "cmd", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(plainSid, ChatMessage{Role: RoleUser, Content: "plain alice"}))

	// Chain: A → B → C, each with words from two authors.
	aSid, err := sessionMgr.CreateSession("net", "#c", alice, "cmd", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(aSid, ChatMessage{Role: RoleUser, Content: "A-words"}))

	bSid, err := cloneDBSession(aSid, "net", "#c", bob, "", "alice")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(bSid, ChatMessage{Role: RoleUser, Content: "B-words"}))

	cSid, err := cloneDBSession(bSid, "net", "#c", carol, "", "bob")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(cSid, ChatMessage{Role: RoleUser, Content: "C-words"}))

	// Reset every message to the pre-migration shape. Clone-copied rows
	// keep created_at = clone time, so the inherit window splits them
	// from the post-clone AddMessage rows written later.
	for _, sid := range []int64{plainSid, aSid, bSid, cSid} {
		legacyRows(t, sid)
	}
	// Guarantee the post-clone rows sit past the inherit window even on
	// a fast test machine: nudge their created_at forward in Go (raw SQL
	// arithmetic mangles sqlite TEXT timestamps into ints).
	var post []Message
	require.NoError(t, theDB.Where(
		"(session_id = ? AND content = 'B-words') OR (session_id = ? AND content = 'C-words')",
		bSid, cSid).Find(&post).Error)
	require.Len(t, post, 2)
	for _, m := range post {
		require.NoError(t, theDB.Model(&Message{}).Where("id = ?", m.ID).
			Update("created_at", m.CreatedAt.Add(10*time.Second)).Error)
	}

	require.NoError(t, backfillMessageUserIDs(theDB))
	require.NoError(t, backfillMessageUserIDs(theDB)) // idempotent

	expect := map[int64]map[string]int64{
		plainSid: {"plain alice": alice},
		aSid:     {"A-words": alice},
		bSid:     {"A-words": alice, "B-words": bob},
		cSid:     {"A-words": alice, "B-words": bob, "C-words": carol},
	}
	for sid, rows := range expect {
		var msgs []Message
		require.NoError(t, theDB.Where("session_id = ? AND role = ?", sid, RoleUser).Find(&msgs).Error)
		require.Len(t, msgs, len(rows), "session %d", sid)
		for _, m := range msgs {
			require.NotNil(t, m.UserID, "session %d row %q left unattributed", sid, m.Content)
			assert.Equal(t, rows[m.Content], *m.UserID, "session %d row %q", sid, m.Content)
		}
	}
}

// TestBackfillCompactionTailCopiesStayNull pins the review fix: a
// pre-migration compaction of a CLONE re-inserted preserved-tail rows
// (possibly the source's inherited words) with fresh created_at past
// the inherit window — the cloner update must NOT stamp those (wrong,
// one-shot, unhealable); they stay NULL. The archived originals keep
// their original timestamps and align correctly, so nothing visible is
// lost (the site never renders tail-copies).
func TestBackfillCompactionTailCopiesStayNull(t *testing.T) {
	setupTestDB(t)

	alice := ensureTestUser(t, "net", "alice")
	bob := ensureTestUser(t, "net", "bob")
	aSid, err := sessionMgr.CreateSession("net", "#c", alice, "cmd", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(aSid, ChatMessage{Role: RoleUser, Content: "A-words"}))
	bSid, err := cloneDBSession(aSid, "net", "#c", bob, "", "alice")
	require.NoError(t, err)
	legacyRows(t, aSid)
	legacyRows(t, bSid)

	// The pre-migration compaction shape: a preserved-tail re-insert of
	// the inherited row, created AFTER the window, tagged as a tail-copy.
	cloneSess, err := sessionMgr.GetSession(bSid)
	require.NoError(t, err)
	compID := int64(1)
	tailRow := Message{
		SessionID: bSid, Role: RoleUser, Content: "A-words",
		SourceCompactionID: &compID,
		CreatedAt:          cloneSess.CreatedAt.Add(cloneInheritWindow + 5*time.Second),
	}
	require.NoError(t, theDB.Create(&tailRow).Error)

	require.NoError(t, backfillMessageUserIDs(theDB))

	var rows []Message
	require.NoError(t, theDB.Where("session_id = ? AND role = ?", bSid, RoleUser).Order("id ASC").Find(&rows).Error)
	require.Len(t, rows, 2)
	require.NotNil(t, rows[0].UserID, "the archived original (in-window) aligns to alice")
	assert.Equal(t, alice, *rows[0].UserID)
	assert.Nil(t, rows[1].UserID, "the tail-copy re-insert must stay NULL — never stamped with the cloner")
}

// TestBackfillMissingSourceLeavesNull: a clone whose source is TRULY
// gone (session row AND its messages) never guesses — inherited rows
// stay NULL. A session row deleted while its messages survive still
// attributes correctly through the surviving rows (covered implicitly
// by the alignment path in TestBackfillMessageUserIDs).
func TestBackfillMissingSourceLeavesNull(t *testing.T) {
	setupTestDB(t)

	alice := ensureTestUser(t, "net", "alice")
	bob := ensureTestUser(t, "net", "bob")
	aSid, err := sessionMgr.CreateSession("net", "#c", alice, "cmd", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(aSid, ChatMessage{Role: RoleUser, Content: "orphan origin"}))

	bSid, err := cloneDBSession(aSid, "net", "#c", bob, "", "alice")
	require.NoError(t, err)
	legacyRows(t, bSid)

	// Hard-delete the source session AND its messages.
	require.NoError(t, theDB.Unscoped().Delete(&Session{}, aSid).Error)
	require.NoError(t, theDB.Unscoped().Where("session_id = ?", aSid).Delete(&Message{}).Error)

	require.NoError(t, backfillMessageUserIDs(theDB))

	var msgs []Message
	require.NoError(t, theDB.Where("session_id = ? AND role = ?", bSid, RoleUser).Find(&msgs).Error)
	require.Len(t, msgs, 1)
	assert.Nil(t, msgs[0].UserID, "truly-missing source: attribution stays NULL, never guessed")
}

// TestBackfillSoftDeletedSourceStillResolves: soft-deleted sources
// remain authoritative (Unscoped read).
func TestBackfillSoftDeletedSourceStillResolves(t *testing.T) {
	setupTestDB(t)

	alice := ensureTestUser(t, "net", "alice")
	bob := ensureTestUser(t, "net", "bob")
	aSid, err := sessionMgr.CreateSession("net", "#c", alice, "cmd", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(aSid, ChatMessage{Role: RoleUser, Content: "soft origin"}))
	bSid, err := cloneDBSession(aSid, "net", "#c", bob, "", "alice")
	require.NoError(t, err)
	legacyRows(t, bSid)
	legacyRows(t, aSid) // source's own rows are legacy too

	require.NoError(t, theDB.Delete(&Session{}, aSid).Error) // soft delete
	require.NoError(t, backfillMessageUserIDs(theDB))

	var msgs []Message
	require.NoError(t, theDB.Where("session_id = ? AND role = ?", bSid, RoleUser).Find(&msgs).Error)
	require.Len(t, msgs, 1)
	require.NotNil(t, msgs[0].UserID)
	assert.Equal(t, alice, *msgs[0].UserID)
}
