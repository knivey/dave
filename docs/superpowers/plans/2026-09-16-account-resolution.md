# Account-Aware Identity Resolution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix identity resolution so IRC accounts are read race-free from event payloads, late-arriving account info (WHOX/ACCOUNT/CHGHOST) triggers re-resolution, and account-bound rows can't be inherited via shared hosts or released nicks.

**Architecture:** dave reads the account directly from girc event payloads (extended-join params, `@account` tags, ACCOUNT params) instead of racing against girc's internal state. JOINs carrying no account info defer resolution to the WHOX (354) reply via a pending map. Account eligibility rules are added to host recovery and the released-nick fallback.

**Tech Stack:** Go 1.25, girc v1.1.1, GORM, testify.

**Spec:** `docs/superpowers/specs/2026-09-16-account-resolution-design.md`

---

### Task 1: `accountFromEvent` helper + event-based `resolveIRCUser`

**Files:**
- Modify: `main.go:75-82` (resolveIRCUser), new helper below it
- Modify: `irc_handlers.go:210,291,370`, `mcpCmds.go:54`, `aiCmds.go:1356`, `historyCmds.go:84,108,672,733` (call sites)
- Create: `main_test.go`

- [ ] **Step 1: Write the failing tests**

Create `main_test.go`:

```go
package main

import (
	"testing"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
)

func TestAccountFromEvent(t *testing.T) {
	client := girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})

	t.Run("extended-join param", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "shrew", "Ron"}}
		assert.Equal(t, "shrew", accountFromEvent(client, e))
	})

	t.Run("extended-join star means unauthenticated", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "*", "Ron"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})

	t.Run("account-notify param", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_ACCOUNT, Source: &girc.Source{Name: "shrew"}, Params: []string{"shrew"}}
		assert.Equal(t, "shrew", accountFromEvent(client, e))
	})

	t.Run("account-notify logout", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_ACCOUNT, Source: &girc.Source{Name: "shrew"}, Params: []string{"*"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})

	t.Run("account message tag", func(t *testing.T) {
		e := girc.Event{Command: girc.PRIVMSG, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "lol"}}
		e.Tags = girc.Tags{"account": "shrew"}
		assert.Equal(t, "shrew", accountFromEvent(client, e))
	})

	t.Run("no account info falls back to empty girc state", func(t *testing.T) {
		e := girc.Event{Command: girc.PRIVMSG, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "lol"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})

	t.Run("nil source does not panic", func(t *testing.T) {
		e := girc.Event{Command: girc.PRIVMSG, Params: []string{"#gay", "lol"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestAccountFromEvent ./...`
Expected: FAIL — `undefined: accountFromEvent`

- [ ] **Step 3: Implement**

In `main.go`, replace `resolveIRCUser` (lines 75-82) with:

```go
func resolveIRCUser(network Network, c *girc.Client, event girc.Event) (*User, error) {
	if event.Source == nil {
		return nil, fmt.Errorf("resolveIRCUser: event has no source")
	}
	casemapping := getCasemapping(network.Name)
	account := accountFromEvent(c, event)
	return resolveUser(network.Name, event.Source.Name, event.Source.Ident, event.Source.Host, account, casemapping)
}

// accountFromEvent extracts the IRC services account for an event's source
// user from the most reliable race-free source available.
//
// DESIGN NOTE: girc dispatches ALL handlers for an event concurrently —
// internal handlers included (handler.go exec() launches every handler as a
// goroutine, "no specific order/priority"). Reading
// client.LookupUser(...).Extras.Account therefore races against girc's own
// state tracking (handleJOIN/handleACCOUNT/handleWHO populating that field).
// For JOIN and ACCOUNT events the account is in the event payload itself;
// for tagged messages the @account tag is too. Those sources are read first;
// the racy state lookup is the last resort for paths where the account was
// learned from an earlier WHOX reply.
func accountFromEvent(c *girc.Client, e girc.Event) string {
	if e.Command == girc.JOIN && len(e.Params) >= 2 {
		// extended-join: Params[1] is the account, "*" means not logged in.
		// The param is authoritative — do not fall through to state.
		if e.Params[1] != "*" {
			return e.Params[1]
		}
		return ""
	}
	if e.Command == girc.CAP_ACCOUNT && len(e.Params) == 1 {
		// account-notify: "*" means logged out.
		if e.Params[0] != "*" {
			return e.Params[0]
		}
		return ""
	}
	if tag, ok := e.Tags.Get("account"); ok && tag != "" && tag != "*" {
		return tag
	}
	if e.Source != nil {
		if u := c.LookupUser(e.Source.Name); u != nil {
			return u.Extras.Account
		}
	}
	return ""
}
```

Update all call sites (mechanical — each has the event in scope):

- `irc_handlers.go:131` (JOIN handler): `resolveIRCUser(network, client, nick, event.Source)` → `resolveIRCUser(network, client, event)`
- `irc_handlers.go:210`: `resolveIRCUser(network, client, event.Source.Name, event.Source)` → `resolveIRCUser(network, client, event)`
- `irc_handlers.go:291`: same replacement as 210
- `irc_handlers.go:370`: same replacement as 210
- `mcpCmds.go:54`: `resolveIRCUser(network, c, nick, e.Source)` → `resolveIRCUser(network, c, e)` (keep the `nick` variable if used later in the function)
- `aiCmds.go:1356`: `resolveIRCUser(network, c, nick, e.Source)` → `resolveIRCUser(network, c, e)`
- `historyCmds.go:84`: `resolveIRCUser(network, c, e.Source.Name, e.Source)` → `resolveIRCUser(network, c, e)`
- `historyCmds.go:108`: same
- `historyCmds.go:672`: same
- `historyCmds.go:733`: same

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test -run TestAccountFromEvent .`
Expected: BUILD OK, PASS

Run: `go test ./...`
Expected: all PASS (signature change is mechanical; no behavior change yet)

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go irc_handlers.go mcpCmds.go aiCmds.go historyCmds.go
git commit -m "refactor: source IRC account from event payload, not girc state"
```

---

### Task 2: Account eligibility in host recovery

**Files:**
- Modify: `users.go:516-572` (`recoverByKnownHost`) and its two call sites (`users.go:304`, `users.go:347`)
- Test: `users_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `users_test.go`:

```go
// TestRecoverByKnownHostAccountEligibility covers the shared-cloak conflation
// fix: a row bound to an IRC services account can only be recovered via host
// by the same account.
func TestRecoverByKnownHostAccountEligibility(t *testing.T) {
	setupTestDB(t)

	owner, err := createNewUser("testnet", "Owner", "owner", "shrew", "~u", "cloak.example")
	require.NoError(t, err)

	t.Run("unauthed stranger does not inherit account-bound row", func(t *testing.T) {
		resolved, err := resolveUser("testnet", "Stranger", "~u", "cloak.example", "", "rfc1459")
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.NotEqual(t, owner.ID, resolved.ID)
	})

	t.Run("differently authed user does not inherit account-bound row", func(t *testing.T) {
		resolved, err := resolveUser("testnet", "Other", "~u", "cloak.example", "someoneelse", "rfc1459")
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.NotEqual(t, owner.ID, resolved.ID)
	})

	t.Run("same account still recovers via host", func(t *testing.T) {
		// Direct call: resolveUser's account branch would find the row by
		// account before host recovery ever runs.
		user, err := recoverByKnownHost("testnet", "~u", "cloak.example", "whatever", "shrew")
		require.NoError(t, err)
		require.NotNil(t, user)
		assert.Equal(t, owner.ID, user.ID)
	})

	t.Run("account-less rows still recover", func(t *testing.T) {
		plain, err := createNewUser("testnet", "Plain", "plain", "", "~u", "other.example")
		require.NoError(t, err)
		resolved, err := resolveUser("testnet", "NewNick", "~u", "other.example", "", "rfc1459")
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.Equal(t, plain.ID, resolved.ID)
	})

	t.Run("multi-match filters account-bound rows before disambiguation", func(t *testing.T) {
		// Two rows share the host: one account-bound, one with nick_changes
		// history matching the incoming nick. Disambiguation must only see
		// the eligible row.
		hist, err := createNewUser("testnet", "HistUser", "histuser", "", "~u", "multi.example")
		require.NoError(t, err)
		_ = hist
		require.NoError(t, upsertKnownHost(owner.ID, "~u", "multi.example"))
		require.NoError(t, upsertKnownHost(hist.ID, "~u", "multi.example"))
		require.True(t, recordNickChange("testnet", "HistUser", "HistAlt", "rfc1459"))

		resolved, err := resolveUser("testnet", "HistAlt", "~u", "multi.example", "", "rfc1459")
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.Equal(t, hist.ID, resolved.ID, "must resolve to the eligible row, not the account-bound one")
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestRecoverByKnownHostAccountEligibility .`
Expected: COMPILE FAIL — `recoverByKnownHost` takes 4 args, test passes 5

- [ ] **Step 3: Implement**

In `users.go`, replace `recoverByKnownHost` (lines 516-572) with:

```go
// recoverByKnownHost attempts to re-associate a user via ident@host when the
// nick is not recognized (bot restart scenario). If ident@host matches
// multiple users, cross-references the normalized nick against nick_changes
// history to disambiguate. Returns nil if no match or ambiguous.
//
// Account eligibility: a candidate row bound to an IRC services account
// (IRCAccount != "") is only recoverable by an incoming user with the SAME
// account. This blocks shared-host conflation (shared cloaks/vhosts with
// coerced idents like ~u): an unauthed or differently-authed stranger on the
// same host gets a fresh row instead of inheriting the account owner's
// sessions, bans, and history.
//
// Flagged users are excluded from the JOIN — they are diagnostic placeholders
// awaiting admin cleanup and must never be matched as a canonical identity.
// Without this filter, a flagged row created via resolveUserFallback would
// inherit the legitimate owner's (ident, host) via upsertKnownHost and could
// then be re-surfaced here, causing the next claimNickFor pass to displace
// or merge real users into the flagged row.
//
// Released users ARE included: if a user quit and is coming back from the
// same ident@host, we want to re-attach to their existing row. The caller
// (resolveUserOnce) clears Released=false on match.
func recoverByKnownHost(network, ident, host, normalizedNick, account string) (*User, error) {
	var hosts []UserKnownHost
	err := theDB.Joins("JOIN users ON users.id = user_known_hosts.user_id").
		Where("users.network = ? AND user_known_hosts.ident = ? AND user_known_hosts.host = ? AND users.flagged = ?",
			network, ident, host, false).
		Find(&hosts).Error
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		loggerUsers.Debug("host recovery: no ident@host match", "ident", ident, "host", host, "nick", normalizedNick, "network", network)
		return nil, nil
	}

	var candidates []*User
	for _, h := range hosts {
		var user User
		if err := theDB.First(&user, h.UserID).Error; err != nil {
			return nil, err
		}
		if user.IRCAccount != "" && user.IRCAccount != account {
			loggerUsers.Debug("host recovery: skipping account-bound row",
				"user_id", user.ID, "row_account", user.IRCAccount,
				"incoming_account", account, "ident", ident, "host", host,
				"nick", normalizedNick, "network", network)
			continue
		}
		candidates = append(candidates, &user)
	}
	if len(candidates) == 0 {
		loggerUsers.Debug("host recovery: no eligible ident@host match (account rule)",
			"ident", ident, "host", host, "nick", normalizedNick, "network", network)
		return nil, nil
	}
	if len(candidates) == 1 {
		loggerUsers.Debug("host recovery: single match", "user_id", candidates[0].ID, "ident", ident, "host", host, "nick", normalizedNick, "network", network)
		return candidates[0], nil
	}

	loggerUsers.Debug("host recovery: multiple matches, disambiguating via nick_changes", "count", len(candidates), "ident", ident, "host", host, "nick", normalizedNick, "network", network)
	for _, c := range candidates {
		var count int64
		theDB.Model(&NickChange{}).
			Where("user_id = ? AND (normalized_old = ? OR normalized_new = ?)",
				c.ID, normalizedNick, normalizedNick).
			Count(&count)
		if count > 0 {
			loggerUsers.Debug("host recovery: disambiguated via nick_changes", "user_id", c.ID, "nick_changes_count", count, "network", network)
			return c, nil
		}
	}

	loggerUsers.Debug("host recovery: ambiguous, no nick_change match for any candidate", "ident", ident, "host", host, "nick", normalizedNick, "network", network)
	return nil, nil
}
```

Update the two call sites in `resolveUserOnce`:
- `users.go:304`: `recoverByKnownHost(network, ident, host, norm)` → `recoverByKnownHost(network, ident, host, norm, account)`
- `users.go:347`: `recoverByKnownHost(network, ident, host, norm)` → `recoverByKnownHost(network, ident, host, norm, "")`

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestRecoverByKnownHost|TestResolveUserByKnownHost' .`
Expected: PASS (old host-recovery tests use account-less rows and are unaffected)

Run: `go test ./...`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add users.go users_test.go
git commit -m "fix: host recovery skips rows bound to a different IRC account"
```

---

### Task 3: Account eligibility in the released-nick fallback

**Files:**
- Modify: `users.go:470-514` (`getMostRecentReleasedUserByNormalizedNick`), `users.go:239-266` (`tryReleasedNickFallback` — pass-through only)
- Test: `users_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `users_test.go`:

```go
// TestReleasedNickFallbackAccountEligibility covers the security fix: a
// released row bound to an account can only be reactivated by the same
// account. Unauthed nick-reusers get fresh rows.
func TestReleasedNickFallbackAccountEligibility(t *testing.T) {
	setupTestDB(t)

	now := time.Now()
	bound := &User{
		Network:        "net",
		CurrentNick:    "shrew",
		NormalizedNick: "shrew",
		IRCAccount:     "shrew",
		Released:       true,
		CreatedAt:      now.Add(-1 * time.Hour),
		UpdatedAt:      now.Add(-1 * time.Hour),
	}
	require.NoError(t, theDB.Create(bound).Error)

	t.Run("unauthed nick reuser gets a fresh row", func(t *testing.T) {
		resolved, err := resolveUser("net", "shrew", "~u", "brandnewhost", "", "rfc1459")
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.NotEqual(t, bound.ID, resolved.ID)
		assert.Equal(t, "", resolved.IRCAccount)

		// The bound row stays released.
		var reloaded User
		require.NoError(t, theDB.First(&reloaded, bound.ID).Error)
		assert.True(t, reloaded.Released, "account-bound released row must stay released")
	})

	t.Run("same account reactivates", func(t *testing.T) {
		user, count, err := getMostRecentReleasedUserByNormalizedNick("net", "shrew", "shrew")
		require.NoError(t, err)
		require.NotNil(t, user)
		assert.Equal(t, bound.ID, user.ID)
		assert.Equal(t, int64(1), count)
	})

	t.Run("different account does not reactivate", func(t *testing.T) {
		user, _, err := getMostRecentReleasedUserByNormalizedNick("net", "shrew", "someoneelse")
		require.NoError(t, err)
		assert.Nil(t, user)
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestReleasedNickFallbackAccountEligibility .`
Expected: COMPILE FAIL — `getMostRecentReleasedUserByNormalizedNick` takes 2 args, test passes 3

- [ ] **Step 3: Implement**

In `users.go`, replace `getMostRecentReleasedUserByNormalizedNick` (lines 470-514) with:

```go
// getMostRecentReleasedUserByNormalizedNick returns the released user with
// the most recent updated_at holding `normalizedNick` on `network`. Flagged
// rows are excluded — they are diagnostic placeholders, not identity matches.
//
// This is the third-tier identity fallback used by resolveUserOnce when both
// active nick lookup and host recovery miss. It exists for the common case
// on accountless networks where a user quits and rejoins from a new host
// (mobile networks, ISP DHCP, VPN cycling): nick alone is enough evidence
// to re-attach to the previous row rather than create a duplicate.
//
// Account eligibility: a released row bound to an IRC services account can
// only be reactivated by an incoming user with the same account. This
// closes the documented security hole where anyone re-using a released nick
// inherited the previous owner's sessions/bans/history.
//
// If more than one released row matches, returns the newest by updated_at
// and the total match count so the caller can WARN about ambiguity. This
// happens after multiple release/reclaim cycles without account or host
// evidence linking them together — the bot is making a best-effort guess.
//
// Returns (nil, 0, nil) when there are no matches.
func getMostRecentReleasedUserByNormalizedNick(network, normalizedNick, account string) (*User, int64, error) {
	where := "network = ? AND normalized_nick = ? AND released = ? AND flagged = ?"
	args := []interface{}{network, normalizedNick, true, false}
	if account == "" {
		where += " AND (account IS NULL OR account = '')"
	} else {
		where += " AND (account IS NULL OR account = '' OR account = ?)"
		args = append(args, account)
	}

	var count int64
	err := theDB.Model(&User{}).Where(where, args...).Count(&count).Error
	if err != nil {
		return nil, 0, err
	}
	if count == 0 {
		return nil, 0, nil
	}
	var user User
	err = theDB.Where(where, args...).Order("updated_at DESC, id DESC").First(&user).Error
	if err != nil {
		return nil, count, err
	}
	return &user, count, nil
}
```

In `tryReleasedNickFallback` (`users.go:240`), pass the account through:

```go
	match, matchCount, err := getMostRecentReleasedUserByNormalizedNick(network, normalizedNick, account)
```

Also delete the now-stale paragraph from the old doc comment above it (the "Security note: ... Full mitigation is deferred to the Phase 5 account system" text was on `getMostRecentReleasedUserByNormalizedNick`; the replacement comment above documents the new behavior).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestReleasedNickFallback|TestResolveUserReleasedNick|TestResolveUserRecoversReleasedNick' .`
Expected: PASS — existing released-fallback tests stage account-less rows (`TestResolveUserReleasedNickFallback_AccountBranch` stages a released row WITHOUT an account and resolves WITH one, which remains eligible)

Run: `go test ./...`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add users.go users_test.go
git commit -m "fix: released account-bound rows only reactivate for same account"
```

---

### Task 4: Pending join-WHO map

**Files:**
- Modify: `irc_handlers.go` (add map + helpers near the top, after `scheduleRejoin`)
- Test: `irc_handlers_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `irc_handlers_test.go`:

```go
func TestPendingJoinWHO(t *testing.T) {
	resetPendingJoinWHO(t)

	t.Run("record then take roundtrip", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "shrew")
		assert.True(t, takePendingJoinWHO("testnet", "shrew"))
		assert.False(t, takePendingJoinWHO("testnet", "shrew"), "entry consumed by first take")
	})

	t.Run("network isolation", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "shrew")
		assert.False(t, takePendingJoinWHO("othernet", "shrew"))
		assert.True(t, takePendingJoinWHO("testnet", "shrew"))
	})

	t.Run("nick isolation", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "shrew")
		assert.False(t, takePendingJoinWHO("testnet", "other"))
	})

	t.Run("expired entries are pruned on record", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "old")
		// Age the entry past the TTL by manipulating the recorded time.
		pendingJoinWHOMu.Lock()
		pendingJoinWHO["testnet"]["old"] = time.Now().Add(-2 * pendingJoinWHOTTL)
		pendingJoinWHOMu.Unlock()

		recordPendingJoinWHO("testnet", "new")
		assert.False(t, takePendingJoinWHO("testnet", "old"), "expired entry must be gone")
		assert.True(t, takePendingJoinWHO("testnet", "new"))
	})

	t.Run("re-record refreshes the timestamp", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "shrew")
		recordPendingJoinWHO("testnet", "shrew")
		pendingJoinWHOMu.Lock()
		assert.Len(t, pendingJoinWHO["testnet"], 1, "no duplicate entries")
		pendingJoinWHOMu.Unlock()
	})
}

func resetPendingJoinWHO(t *testing.T) {
	t.Helper()
	pendingJoinWHOMu.Lock()
	pendingJoinWHO = map[string]map[string]time.Time{}
	pendingJoinWHOMu.Unlock()
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestPendingJoinWHO .`
Expected: COMPILE FAIL — `undefined: recordPendingJoinWHO`

- [ ] **Step 3: Implement**

Add to `irc_handlers.go` after the `scheduleRejoin` var (line ~35), and add `"sync"` and `"time"` to imports if missing (`time` is already imported):

```go
// pendingJoinWHO tracks JOINs that could not be resolved at join time because
// the connection lacks extended-join / account-tag (the event carried no
// account info). girc's builtin sends `WHO <nick> %tacuhnr,1` for every
// foreign JOIN; the 354 reply completes the deferred resolution. Entries are
// consumed by the reply or expire (server without WHOX support, netsplit),
// so a TTL prevents unbounded growth.
const pendingJoinWHOTTL = 60 * time.Second

var pendingJoinWHOMu sync.Mutex
var pendingJoinWHO = map[string]map[string]time.Time{}

// recordPendingJoinWHO registers a deferred JOIN resolution. Also prunes
// expired entries from all networks so the map cannot grow without bound.
func recordPendingJoinWHO(network, normNick string) {
	pendingJoinWHOMu.Lock()
	defer pendingJoinWHOMu.Unlock()
	now := time.Now()
	for net, nicks := range pendingJoinWHO {
		for nick, at := range nicks {
			if now.Sub(at) > pendingJoinWHOTTL {
				delete(nicks, nick)
			}
		}
		if len(nicks) == 0 {
			delete(pendingJoinWHO, net)
		}
	}
	netMap := pendingJoinWHO[network]
	if netMap == nil {
		netMap = map[string]time.Time{}
		pendingJoinWHO[network] = netMap
	}
	netMap[normNick] = now
}

// takePendingJoinWHO reports whether a deferred JOIN resolution is pending
// for the nick and removes the entry (one WHOX reply completes one deferral).
func takePendingJoinWHO(network, normNick string) bool {
	pendingJoinWHOMu.Lock()
	defer pendingJoinWHOMu.Unlock()
	netMap, ok := pendingJoinWHO[network]
	if !ok {
		return false
	}
	if _, ok := netMap[normNick]; !ok {
		return false
	}
	delete(netMap, normNick)
	if len(netMap) == 0 {
		delete(pendingJoinWHO, network)
	}
	return true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestPendingJoinWHO .`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add irc_handlers.go irc_handlers_test.go
git commit -m "feat: pending join-WHO map for deferred join resolution"
```

---

### Task 5: `handleUserJoin` — immediate resolve or defer

**Files:**
- Modify: `irc_handlers.go:126-135` (JOIN handler)
- Test: `irc_handlers_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `irc_handlers_test.go` (imports gain `"github.com/lrstanley/girc"` and `"github.com/stretchr/testify/require"` if missing):

```go
func TestHandleUserJoin(t *testing.T) {
	resetPendingJoinWHO(t)
	setupTestDB(t)

	client := girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})
	network := Network{Name: "testnet"}

	countUsers := func() int64 {
		var n int64
		theDB.Model(&User{}).Count(&n)
		return n
	}

	t.Run("extended-join account resolves immediately", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "shrew", Ident: "~u", Host: "cloak.example"}, Params: []string{"#gay", "shrew", "Ron"}}
		handleUserJoin(network, client, e, newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "shrew").First(&user).Error)
		assert.Equal(t, "shrew", user.IRCAccount)
	})

	t.Run("extended-join star resolves immediately without account", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "anon", Ident: "~u", Host: "cloak.example"}, Params: []string{"#gay", "*", "Ron"}}
		handleUserJoin(network, client, e, newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "anon").First(&user).Error)
		assert.Equal(t, "", user.IRCAccount)
	})

	t.Run("no account info defers resolution", func(t *testing.T) {
		before := countUsers()
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "deferred", Ident: "~u", Host: "cloak.example"}, Params: []string{"#gay"}}
		handleUserJoin(network, client, e, newTestLogger())

		assert.Equal(t, before, countUsers(), "no user row must be created yet")
		assert.True(t, takePendingJoinWHO("testnet", "deferred"))
	})

	t.Run("account tag resolves immediately", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "tagged", Ident: "~u", Host: "cloak.example"}, Params: []string{"#gay"}}
		e.Tags = girc.Tags{"account": "tagacct"}
		handleUserJoin(network, client, e, newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "tagged").First(&user).Error)
		assert.Equal(t, "tagacct", user.IRCAccount)
	})

	t.Run("bot's own join is ignored", func(t *testing.T) {
		before := countUsers()
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "testbot", Ident: "~u", Host: "bot.example"}, Params: []string{"#gay", "botacct", "Bot"}}
		handleUserJoin(network, client, e, newTestLogger())
		assert.Equal(t, before, countUsers())
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestHandleUserJoin .`
Expected: COMPILE FAIL — `undefined: handleUserJoin`

- [ ] **Step 3: Implement**

Replace the JOIN handler closure in `registerIRCHandlers` (`irc_handlers.go:126-135`) with a call to a new extracted function:

```go
	client.Handlers.Add(girc.JOIN, func(client *girc.Client, event girc.Event) {
		handleUserJoin(network, client, event, log)
	})
```

And add the function (place it after `registerIRCHandlers`):

```go
// handleUserJoin resolves a joining user, deferring to the WHOX reply when
// the JOIN event carries no account information.
//
// DESIGN NOTE: account info comes from the event payload (extended-join
// params / @account tag), never from client.LookupUser — girc runs all
// handlers for an event concurrently, so its internal state population races
// with ours. When the event has no account info (connection without
// extended-join and without account-tag), no row is created at join time;
// the resolution is deferred to the 354 reply for girc's per-join
// `WHO <nick> %tacuhnr,1` (see handleWHOXReply). On servers without WHOX
// support the deferral simply expires and the user is created on first
// interaction.
func handleUserJoin(network Network, client *girc.Client, event girc.Event, log logxi.Logger) {
	if event.Source == nil {
		return
	}
	nick := event.Source.Name
	if nick == client.GetNick() {
		return
	}
	account := accountFromEvent(client, event)
	if len(event.Params) >= 2 || account != "" {
		_, err := resolveIRCUser(network, client, event)
		if err != nil {
			log.Error("failed to resolve user on join", "nick", nick, "error", err)
		}
		return
	}
	recordPendingJoinWHO(network.Name, normalizeIRC(nick, getCasemapping(network.Name)))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestHandleUserJoin .`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add irc_handlers.go irc_handlers_test.go
git commit -m "feat: join resolution uses event account, defers to whox otherwise"
```

---

### Task 6: `handleWHOXReply` (354)

**Files:**
- Modify: `irc_handlers.go` (new function; registration happens in Task 9)
- Test: `irc_handlers_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `irc_handlers_test.go`:

```go
func TestHandleWHOXReply(t *testing.T) {
	resetPendingJoinWHO(t)
	setupTestDB(t)

	network := Network{Name: "testnet"}
	whoxEvent := func(nick, ident, host, account string) girc.Event {
		return girc.Event{
			Command: girc.RPL_WHOSPCRPL,
			Params:  []string{"testbot", "1", "#gay", ident, host, nick, account, "Real Name"},
		}
	}

	t.Run("pending nick resolves with whox account", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "whouser")
		handleWHOXReply(network, whoxEvent("whoUser", "~u", "cloak.example", "whoacct"), newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "whouser").First(&user).Error)
		assert.Equal(t, "whoacct", user.IRCAccount)
		assert.False(t, takePendingJoinWHO("testnet", "whouser"), "entry consumed")
	})

	t.Run("whox 0 means unauthenticated", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "zero")
		handleWHOXReply(network, whoxEvent("Zero", "~u", "cloak.example", "0"), newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "zero").First(&user).Error)
		assert.Equal(t, "", user.IRCAccount)
	})

	t.Run("non-pending nick is ignored", func(t *testing.T) {
		before := int64(0)
		theDB.Model(&User{}).Count(&before)
		handleWHOXReply(network, whoxEvent("Random", "~u", "cloak.example", "acct"), newTestLogger())
		after := int64(0)
		theDB.Model(&User{}).Count(&after)
		assert.Equal(t, before, after, "no row created for non-pending nick")
	})

	t.Run("wrong querytype is ignored, pending preserved", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "qt")
		e := girc.Event{Command: girc.RPL_WHOSPCRPL, Params: []string{"testbot", "9", "#gay", "~u", "cloak.example", "QT", "acct", "Real"}}
		handleWHOXReply(network, e, newTestLogger())
		assert.True(t, takePendingJoinWHO("testnet", "qt"), "pending entry must survive")
	})

	t.Run("malformed reply is ignored", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "mal")
		e := girc.Event{Command: girc.RPL_WHOSPCRPL, Params: []string{"testbot", "1"}}
		handleWHOXReply(network, e, newTestLogger())
		assert.True(t, takePendingJoinWHO("testnet", "mal"), "pending entry must survive")
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestHandleWHOXReply .`
Expected: COMPILE FAIL — `undefined: handleWHOXReply`

- [ ] **Step 3: Implement**

Add to `irc_handlers.go` after `handleUserJoin`:

```go
// handleWHOXReply completes deferred JOIN resolutions when the WHOX (354)
// reply for a pending nick arrives. girc's builtin sends
// `WHO <nick> %tacuhnr,1` for every foreign JOIN, so replies have the
// 8-param layout: <me> <querytype> <channel> <ident> <host> <nick>
// <account> :<realname>. Replies for non-pending nicks (e.g. the
// channel-wide WHO from the bot's own joins) are ignored.
func handleWHOXReply(network Network, event girc.Event, log logxi.Logger) {
	if len(event.Params) != 8 || event.Params[1] != "1" {
		return
	}
	nick := event.Params[5]
	casemapping := getCasemapping(network.Name)
	norm := normalizeIRC(nick, casemapping)
	if !takePendingJoinWHO(network.Name, norm) {
		return
	}
	account := event.Params[6]
	if account == "0" || account == "*" {
		account = ""
	}
	if _, err := resolveUser(network.Name, nick, event.Params[3], event.Params[4], account, casemapping); err != nil {
		log.Error("failed to resolve user on whox reply", "nick", nick, "error", err)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestHandleWHOXReply .`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add irc_handlers.go irc_handlers_test.go
git commit -m "feat: whox 354 reply completes deferred join resolutions"
```

---

### Task 7: `handleAccountChange` (ACCOUNT)

**Files:**
- Modify: `irc_handlers.go` (new function; registration happens in Task 9)
- Test: `irc_handlers_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `irc_handlers_test.go`:

```go
func TestHandleAccountChange(t *testing.T) {
	setupTestDB(t)

	client := girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})
	network := Network{Name: "testnet"}
	acctEvent := func(nick, account string) girc.Event {
		return girc.Event{
			Command: girc.CAP_ACCOUNT,
			Source:  &girc.Source{Name: nick, Ident: "~u", Host: "cloak.example"},
			Params:  []string{account},
		}
	}

	t.Run("account attaches to active row holding the nick", func(t *testing.T) {
		ghost, err := createNewUser("testnet", "Newbie", "newbie", "", "~u", "cloak.example")
		require.NoError(t, err)

		handleAccountChange(network, client, acctEvent("Newbie", "newacct"), newTestLogger())

		var reloaded User
		require.NoError(t, theDB.First(&reloaded, ghost.ID).Error)
		assert.Equal(t, "newacct", reloaded.IRCAccount)
	})

	t.Run("account claim displaces ghost holding the nick (production incident)", func(t *testing.T) {
		// Real account holder: active under a different nick.
		owner, err := createNewUser("testnet", "again", "again", "shrew", "~u", "old.host")
		require.NoError(t, err)
		// Ghost row created at join time before the account was known.
		ghost, err := createNewUser("testnet", "shrew2", "shrew2", "", "~u", "cloak.example")
		require.NoError(t, err)

		handleAccountChange(network, client, acctEvent("shrew2", "shrew"), newTestLogger())

		var ownerRow, ghostRow User
		require.NoError(t, theDB.First(&ownerRow, owner.ID).Error)
		require.NoError(t, theDB.First(&ghostRow, ghost.ID).Error)
		assert.Equal(t, "shrew2", ownerRow.CurrentNick, "account holder takes the nick")
		assert.True(t, ghostRow.Released, "ghost nick released")
	})

	t.Run("logout is ignored", func(t *testing.T) {
		before := int64(0)
		theDB.Model(&User{}).Count(&before)
		handleAccountChange(network, client, acctEvent("nobody", "*"), newTestLogger())
		after := int64(0)
		theDB.Model(&User{}).Count(&after)
		assert.Equal(t, before, after, "logout must not create or change rows")
	})

	t.Run("malformed event is ignored", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_ACCOUNT, Source: &girc.Source{Name: "x"}, Params: []string{}}
		handleAccountChange(network, client, e, newTestLogger()) // must not panic
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestHandleAccountChange .`
Expected: COMPILE FAIL — `undefined: handleAccountChange`

- [ ] **Step 3: Implement**

Add to `irc_handlers.go` after `handleWHOXReply`:

```go
// handleAccountChange re-resolves a user when the server reports an account
// login (account-notify). This retro-fixes ghosts created before the account
// was known (join happened pre-authentication) — resolveUser's account
// branch finds the real row and claimNickFor displaces any ghost holding
// the nick. Logouts ("*") are ignored: the row keeps its account as the
// identity key and nick lookup still serves subsequent messages.
func handleAccountChange(network Network, client *girc.Client, event girc.Event, log logxi.Logger) {
	if len(event.Params) != 1 || event.Source == nil {
		return
	}
	if event.Params[0] == "*" {
		return
	}
	if _, err := resolveIRCUser(network, client, event); err != nil {
		log.Error("failed to re-resolve user on account change", "nick", event.Source.Name, "error", err)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestHandleAccountChange .`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add irc_handlers.go irc_handlers_test.go
git commit -m "feat: re-resolve user on account-notify login"
```

---

### Task 8: `handleHostChange` (CHGHOST)

**Files:**
- Modify: `irc_handlers.go` (new function; registration happens in Task 9)
- Test: `irc_handlers_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `irc_handlers_test.go`:

```go
func TestHandleHostChange(t *testing.T) {
	setupTestDB(t)

	chgEvent := func(nick, ident, host string) girc.Event {
		return girc.Event{
			Command: girc.CAP_CHGHOST,
			Source:  &girc.Source{Name: nick, Ident: "~old", Host: "old.example"},
			Params:  []string{ident, host},
		}
	}

	t.Run("new ident@host recorded for active user", func(t *testing.T) {
		user, err := createNewUser("testnet", "Mover", "mover", "", "~old", "old.example")
		require.NoError(t, err)

		handleHostChange("testnet", chgEvent("Mover", "~new", "new.example"), newTestLogger())

		var hosts []UserKnownHost
		theDB.Where("user_id = ?", user.ID).Find(&hosts)
		assert.Len(t, hosts, 2, "old and new host both known")
		found := false
		for _, h := range hosts {
			if h.Ident == "~new" && h.Host == "new.example" {
				found = true
			}
		}
		assert.True(t, found, "new host must be recorded")
	})

	t.Run("unknown nick is a no-op", func(t *testing.T) {
		before := int64(0)
		theDB.Model(&UserKnownHost{}).Count(&before)
		handleHostChange("testnet", chgEvent("Stranger", "~x", "x.example"), newTestLogger())
		after := int64(0)
		theDB.Model(&UserKnownHost{}).Count(&after)
		assert.Equal(t, before, after)
	})

	t.Run("malformed event is ignored", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_CHGHOST, Source: &girc.Source{Name: "x"}, Params: []string{"onlyone"}}
		handleHostChange("testnet", e, newTestLogger()) // must not panic
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestHandleHostChange .`
Expected: COMPILE FAIL — `undefined: handleHostChange`

- [ ] **Step 3: Implement**

Add to `irc_handlers.go` after `handleAccountChange`:

```go
// handleHostChange records ident/host changes (chghost cap) into the user's
// known hosts so future host-based recovery works after a vhost/cloak change.
// No re-resolution is needed — identity did not change, only the host. If no
// active row holds the nick (user never interacted), there is nothing to do;
// a pending WHOX deferral carries the fresh host anyway.
func handleHostChange(network string, event girc.Event, log logxi.Logger) {
	if len(event.Params) != 2 || event.Source == nil {
		return
	}
	norm := normalizeIRC(event.Source.Name, getCasemapping(network))
	user, err := getActiveUserByNormalizedNick(network, norm)
	if err != nil {
		log.Error("failed to look up user for chghost", "nick", event.Source.Name, "error", err)
		return
	}
	if user == nil {
		return
	}
	if err := upsertKnownHost(user.ID, event.Params[0], event.Params[1]); err != nil {
		log.Error("failed to upsert known host on chghost", "nick", event.Source.Name, "error", err)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestHandleHostChange .`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add irc_handlers.go irc_handlers_test.go
git commit -m "feat: record chghost ident/host updates in known hosts"
```

---

### Task 9: Wire handlers into `registerIRCHandlers`, update AGENTS.md

**Files:**
- Modify: `irc_handlers.go` (`registerIRCHandlers`)
- Modify: `AGENTS.md`

- [ ] **Step 1: Register the new handlers**

In `registerIRCHandlers` (`irc_handlers.go`), after the existing KICK handler block (line ~175), add:

```go
	client.Handlers.Add(girc.RPL_WHOSPCRPL, func(client *girc.Client, event girc.Event) {
		handleWHOXReply(network, event, log)
	})

	client.Handlers.Add(girc.CAP_ACCOUNT, func(client *girc.Client, event girc.Event) {
		handleAccountChange(network, client, event, log)
	})

	client.Handlers.Add(girc.CAP_CHGHOST, func(client *girc.Client, event girc.Event) {
		handleHostChange(network.Name, event, log)
	})
```

(The JOIN handler closure already calls `handleUserJoin` from Task 5.)

- [ ] **Step 2: Build and run the full suite**

Run: `go build ./... && go fmt ./... && go vet ./... && go test ./...`
Expected: BUILD OK, no fmt diffs, no vet findings, all tests PASS

- [ ] **Step 3: Update AGENTS.md**

In AGENTS.md, update the "Released-nick fallback in `resolveUserOnce`" bullet — replace the sentence fragment `Security note: on zero-trust networks this extends nick continuity across disconnects, so an attacker re-using a released nick inherits the previous owner's sessions/bans/history — same trust posture the bot already had via in-channel nick continuity, just preserved across QUIT/PART/KICK. Full mitigation deferred to Phase 5 account system.` with:

```
Account eligibility: released rows bound to an IRC services account (`account` set) are only reactivated by an incoming user with the same account; unauthed or differently-authed nick reusers get fresh rows. This also applies to `recoverByKnownHost` — host recovery can never match a row bound to a different account (shared-cloak conflation fix).
```

Add a new bullet to "High-Signal Gotchas":

```
- **Account sourcing is event-based, never girc-state-based at join time**: girc runs all handlers for an event concurrently, so `client.LookupUser().Extras.Account` races with girc's internal state tracking. `accountFromEvent` (main.go) reads the account from the event payload (extended-join `Params[1]`, `ACCOUNT` `Params[0]`, `@account` tag) first. JOINs with no account info defer resolution to the WHOX 354 reply (`pendingJoinWHO` map, 60s TTL). `ACCOUNT` logins trigger immediate re-resolution (retro-fixes pre-auth join ghosts); `CHGHOST` updates known hosts.
```

- [ ] **Step 4: Final verification**

Run: `go test ./...`
Expected: all PASS

Run: `go build -o /tmp/opencode/dave-test .`
Expected: builds clean (discard the binary)

- [ ] **Step 5: Commit**

```bash
git add irc_handlers.go AGENTS.md
git commit -m "feat: wire whox/account/chghost handlers; document account resolution"
```

---

## Self-Review Notes

- Spec coverage: account sourcing (Task 1), JOIN immediate/deferred (Tasks 4-5), WHOX completion (Task 6), ACCOUNT re-resolve (Task 7), CHGHOST (Task 8), host-recovery eligibility (Task 2), released-nick eligibility (Task 3), AGENTS.md updates (Task 9). 352 handling intentionally omitted per spec (out of scope).
- Test `TestRecoverByKnownHostAccountEligibility/multi-match` stages `owner` in the outer scope before subtests that create additional rows; account-bound `owner` row's normalized nick ("owner") never collides with test nicks.
- `createNewUser(network, nick, normNick, account, ident, host)` signature matches `users.go:574`.
- `resetPendingJoinWHO` added in Task 4 is reused by Tasks 5-6 tests.
