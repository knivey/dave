package main

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// FlaggedReasonCollision is set on flagged users created because the normal
// resolveUser flow could not claim a real normalized_nick (UNIQUE constraint
// fired even after claimNickFor). See resolveUserFallback for details.
const FlaggedReasonCollision = "collision_unique_nick"

// claimNickForFn is a package-level indirection over claimNickFor so tests
// can override it to force a UNIQUE-constraint failure path. Production code
// always uses the real claimNickFor.
var claimNickForFn = claimNickFor

// resolveUserRand is a goroutine-safe random source used to add jitter to the
// retry backoff in resolveUser. Guarded by resolveUserRandMu because *rand.Rand
// is not goroutine-safe.
var (
	resolveUserRandMu sync.Mutex
	resolveUserRand   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func resolveUserJitter() time.Duration {
	resolveUserRandMu.Lock()
	defer resolveUserRandMu.Unlock()
	return time.Duration(resolveUserRand.Intn(30)) * time.Millisecond
}

// ErrUserResolveTransient wraps a transient DB error (lock contention,
// "database is busy"). After retries exhausted, resolveUser returns this so
// callers can prompt the user to try again.
type ErrUserResolveTransient struct {
	Err error
}

func (e *ErrUserResolveTransient) Error() string {
	if e == nil || e.Err == nil {
		return "transient db error"
	}
	return "transient db error: " + e.Err.Error()
}

func (e *ErrUserResolveTransient) Unwrap() error { return e.Err }

// ErrUserResolveCollision indicates the UNIQUE-constraint path was hit and
// the fallback flagged-row creation itself also failed. Should be very rare;
// callers should send the persistent notice and log loudly.
type ErrUserResolveCollision struct {
	Network        string
	Nick           string
	Account        string
	ExistingUserID int64
	Err            error
}

func (e *ErrUserResolveCollision) Error() string {
	if e == nil {
		return "user resolve collision"
	}
	cause := ""
	if e.Err != nil {
		cause = ": " + e.Err.Error()
	}
	return fmt.Sprintf("user resolve collision (network=%s nick=%s account=%s)%s",
		e.Network, e.Nick, e.Account, cause)
}

func (e *ErrUserResolveCollision) Unwrap() error { return e.Err }

// isUniqueConstraintErr returns true for SQLite UNIQUE-constraint and
// Postgres unique-violation errors. String-matching is used because the
// underlying drivers don't share a typed error API; SQLITE and pg both emit
// these substrings reliably.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") ||
		strings.Contains(s, "duplicate key") ||
		strings.Contains(s, "SQLSTATE 23505")
}

// isTransientDBErr returns true for transient DB errors that may succeed on
// retry: SQLite lock contention, Postgres serialization failure, deadlock.
func isTransientDBErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "database is locked") ||
		strings.Contains(s, "database is busy") ||
		strings.Contains(s, "SQLITE_BUSY") ||
		strings.Contains(s, "deadlock detected") ||
		strings.Contains(s, "could not serialize access")
}

// displayNick returns the display nick for a user. Currently this is just
// CurrentNick — wrapper exists as a future-proofing seam (e.g. to decorate
// released users in some UIs, or to fall back to IRCAccount when set). Use
// this rather than reading CurrentNick directly so future presentation
// changes have one place to land.
func displayNick(u *User) string {
	return u.CurrentNick
}

var loggerUsers = newLogger("users")

// claimNickFor ensures `user` may safely take `(network, norm)` as its
// normalized_nick before the caller assigns it and writes the row. If a
// different *active* user currently holds that slot, handleNickCollision
// either merges them into `user` (when they have no known hosts —
// migration-era ghost) or releases their nick (real user, presumed
// offline/displaced). After this returns nil, the (network, norm) pair is
// guaranteed free for `user` to claim under the partial unique index
// idx_users_nick_active.
//
// Released and flagged rows are not collisions: the partial unique index
// excludes them, so multiple released/flagged rows for the same nick can
// coexist with the active owner.
//
// This mirrors the collision handling recordNickChange performs for NICK
// events. resolveUser needs the same guard because it can identify a user by
// account or by ident@host recovery and then try to assign that user a
// normalized_nick already owned by another active row.
func claimNickFor(network string, user *User, norm string) error {
	// Skip the short-circuit for released/flagged rows: even if the
	// normalized_nick happens to match, the caller is about to reactivate
	// them (clear Released) which moves the row into the partial unique
	// index's active set. We need to verify no other active row holds
	// the slot before letting the caller commit. Do not simplify this
	// to a plain `user.NormalizedNick == norm` check.
	if user.NormalizedNick == norm && !user.Released && !user.Flagged {
		return nil
	}
	existing, err := getActiveUserByNormalizedNick(network, norm)
	if err != nil {
		return err
	}
	if existing == nil || existing.ID == user.ID {
		return nil
	}
	loggerUsers.Debug("claimNickFor: collision detected, delegating to handleNickCollision",
		"claiming_user_id", user.ID,
		"existing_user_id", existing.ID,
		"norm", norm,
		"network", network)
	return handleNickCollision(network, existing, user)
}

// resolveUser finds or creates a User for the given IRC identity.
//
// Resolution priority (per todo.md Phase 2 design):
//  1. IRC account name — if the network provides IRC services account info (girc
//     Extras.Account), match by (network, account). Strongest identity key.
//  2. Nick continuity — match by (network, normalized_nick). Primary method
//     for networks without services. Relies on NICK handler keeping
//     current_nick / normalized_nick up to date.
//  3. ident@host recovery — only used when nick is not recognized (e.g. bot
//     restart). NOT a primary identity method since multiple users can share
//     the same host. If ident@host matches multiple users, the nick is
//     cross-referenced against nick_changes history to disambiguate.
//
// Users are created only on bot interaction (not every channel message).
func resolveUser(network, nick, ident, host, account, casemapping string) (*User, error) {
	if theDB == nil {
		return nil, nil
	}

	backoffs := []time.Duration{50 * time.Millisecond, 150 * time.Millisecond}
	var lastErr error
	for attempt := 0; attempt <= len(backoffs); attempt++ {
		user, err := resolveUserOnce(network, nick, ident, host, account, casemapping)
		if err == nil {
			return user, nil
		}
		lastErr = err
		if !isTransientDBErr(err) {
			break
		}
		if attempt < len(backoffs) {
			time.Sleep(backoffs[attempt] + resolveUserJitter())
			loggerUsers.Debug("resolveUser transient error, retrying",
				"attempt", attempt+1, "error", err.Error(),
				"network", network, "nick", nick)
			continue
		}
		return nil, &ErrUserResolveTransient{Err: err}
	}

	if isUniqueConstraintErr(lastErr) {
		return resolveUserFallback(network, nick, ident, host, account, casemapping, lastErr)
	}
	return nil, lastErr
}

// reactivateIfReleased clears the Released flag on a user that we've just
// identified by a stable key (account or known host). The user has come back
// and is taking their nick again. Returns true if the row was previously
// released and is now being re-activated.
func reactivateIfReleased(user *User) bool {
	if !user.Released {
		return false
	}
	user.Released = false
	loggerUsers.Debug("reactivating released user", "user_id", user.ID, "network", user.Network)
	return true
}

// resolveUserOnce performs a single resolveUser attempt (the original
// pre-retry implementation). Callers should not invoke this directly outside
// of tests; production code uses resolveUser which adds retry + fallback.
func resolveUserOnce(network, nick, ident, host, account, casemapping string) (*User, error) {
	if theDB == nil {
		return nil, nil
	}

	norm := normalizeIRC(nick, casemapping)

	if account != "" {
		user, err := getUserByAccount(network, account)
		if err != nil {
			return nil, err
		}
		if user != nil {
			loggerUsers.Debug("resolved user", "method", "account", "user_id", user.ID, "nick", nick, "account", account, "network", network)
			if err := claimNickForFn(network, user, norm); err != nil {
				return nil, err
			}
			reactivateIfReleased(user)
			user.CurrentNick = nick
			user.NormalizedNick = norm
			if err := updateDBUser(user); err != nil {
				return nil, err
			}
			if err := upsertKnownHost(user.ID, ident, host); err != nil {
				loggerUsers.Warn("failed to upsert known host", "error", err)
			}
			return user, nil
		}

		loggerUsers.Debug("account lookup missed, trying nick lookup", "account", account, "nick", nick, "network", network)
		nickUser, err := getActiveUserByNormalizedNick(network, norm)
		if err != nil {
			return nil, err
		}
		if nickUser != nil {
			loggerUsers.Debug("resolved user", "method", "nick+account_link", "user_id", nickUser.ID, "nick", nick, "account", account, "network", network)
			nickUser.IRCAccount = account
			nickUser.CurrentNick = nick
			nickUser.NormalizedNick = norm
			if err := updateDBUser(nickUser); err != nil {
				return nil, err
			}
			if err := upsertKnownHost(nickUser.ID, ident, host); err != nil {
				loggerUsers.Warn("failed to upsert known host", "error", err)
			}
			return nickUser, nil
		}

		hostUser, err := recoverByKnownHost(network, ident, host, norm)
		if err != nil {
			return nil, err
		}
		if hostUser != nil {
			loggerUsers.Debug("resolved user", "method", "host_recovery+account_link", "user_id", hostUser.ID, "nick", nick, "account", account, "network", network)
			if err := claimNickForFn(network, hostUser, norm); err != nil {
				return nil, err
			}
			reactivateIfReleased(hostUser)
			hostUser.IRCAccount = account
			hostUser.CurrentNick = nick
			hostUser.NormalizedNick = norm
			if err := updateDBUser(hostUser); err != nil {
				return nil, err
			}
			if err := upsertKnownHost(hostUser.ID, ident, host); err != nil {
				loggerUsers.Warn("failed to upsert known host", "error", err)
			}
			return hostUser, nil
		}

		// Third-tier fallback: released row with same nick. Only reached
		// when account is unknown to us AND no host history matches, so
		// nick is the only evidence we have. See
		// getMostRecentReleasedUserByNormalizedNick for the security note.
		//
		// Both voluntarily-released rows (QUIT/PART/KICK) and involuntarily-
		// released rows (handleNickCollision displacement) are eligible.
		// See handleNickCollision docstring for the trade-off rationale.
		//
		// Race note: under high concurrency two callers may try to reactivate
		// two different released rows with the same (network, normalized_nick)
		// simultaneously. One UPDATE will hit the partial UNIQUE constraint
		// and resolveUser will fall through to resolveUserFallback, producing
		// a flagged row. Acceptable degraded behavior; the second user will
		// not be silently merged into the wrong identity.
		releasedUser, matchCount, err := getMostRecentReleasedUserByNormalizedNick(network, norm)
		if err != nil {
			return nil, err
		}
		if releasedUser != nil {
			if matchCount > 1 {
				loggerUsers.Warn("released-nick fallback: multiple released rows match, picking newest",
					"user_id", releasedUser.ID,
					"match_count", matchCount,
					"nick", nick, "network", network)
			}
			loggerUsers.Info("resolved user", "method", "released_nick_fallback+account_link",
				"user_id", releasedUser.ID, "nick", nick, "account", account, "network", network,
				"released_at", releasedUser.UpdatedAt)
			reactivateIfReleased(releasedUser)
			releasedUser.IRCAccount = account
			releasedUser.CurrentNick = nick
			releasedUser.NormalizedNick = norm
			if err := updateDBUser(releasedUser); err != nil {
				return nil, err
			}
			if err := upsertKnownHost(releasedUser.ID, ident, host); err != nil {
				loggerUsers.Warn("failed to upsert known host", "error", err)
			}
			return releasedUser, nil
		}

		return createNewUser(network, nick, norm, account, ident, host)
	}

	user, err := getActiveUserByNormalizedNick(network, norm)
	if err != nil {
		return nil, err
	}
	if user != nil {
		loggerUsers.Debug("resolved user", "method", "nick", "user_id", user.ID, "nick", nick, "network", network)
		if user.CurrentNick != nick {
			user.CurrentNick = nick
			user.NormalizedNick = norm
			if err := updateDBUser(user); err != nil {
				return nil, err
			}
		}
		if err := upsertKnownHost(user.ID, ident, host); err != nil {
			loggerUsers.Warn("failed to upsert known host", "error", err)
		}
		return user, nil
	}

	loggerUsers.Debug("nick lookup missed, trying host recovery", "nick", nick, "ident", ident, "host", host, "network", network)
	hostUser, err := recoverByKnownHost(network, ident, host, norm)
	if err != nil {
		return nil, err
	}
	if hostUser != nil {
		loggerUsers.Debug("resolved user", "method", "host_recovery", "user_id", hostUser.ID, "nick", nick, "network", network)
		if err := claimNickForFn(network, hostUser, norm); err != nil {
			return nil, err
		}
		reactivateIfReleased(hostUser)
		if hostUser.IRCAccount == "" && account != "" {
			hostUser.IRCAccount = account
		}
		hostUser.CurrentNick = nick
		hostUser.NormalizedNick = norm
		if err := updateDBUser(hostUser); err != nil {
			return nil, err
		}
		if err := upsertKnownHost(hostUser.ID, ident, host); err != nil {
			loggerUsers.Warn("failed to upsert known host", "error", err)
		}
		return hostUser, nil
	}

	// Third-tier fallback: released row with same nick. This handles the
	// common accountless-network case where a user quits and rejoins from
	// a new host (mobile networks, ISP DHCP, VPN). See
	// getMostRecentReleasedUserByNormalizedNick for the security note.
	//
	// Both voluntarily-released rows (QUIT/PART/KICK) and involuntarily-
	// released rows (handleNickCollision displacement) are eligible.
	// See handleNickCollision docstring for the trade-off rationale.
	//
	// Race note: under high concurrency two callers may try to reactivate
	// two different released rows with the same (network, normalized_nick)
	// simultaneously. One UPDATE will hit the partial UNIQUE constraint
	// and resolveUser will fall through to resolveUserFallback, producing
	// a flagged row. Acceptable degraded behavior; the second user will
	// not be silently merged into the wrong identity.
	releasedUser, matchCount, err := getMostRecentReleasedUserByNormalizedNick(network, norm)
	if err != nil {
		return nil, err
	}
	if releasedUser != nil {
		if matchCount > 1 {
			loggerUsers.Warn("released-nick fallback: multiple released rows match, picking newest",
				"user_id", releasedUser.ID,
				"match_count", matchCount,
				"nick", nick, "network", network)
		}
		loggerUsers.Info("resolved user", "method", "released_nick_fallback",
			"user_id", releasedUser.ID, "nick", nick, "network", network,
			"released_at", releasedUser.UpdatedAt)
		reactivateIfReleased(releasedUser)
		if releasedUser.IRCAccount == "" && account != "" {
			releasedUser.IRCAccount = account
		}
		releasedUser.CurrentNick = nick
		releasedUser.NormalizedNick = norm
		if err := updateDBUser(releasedUser); err != nil {
			return nil, err
		}
		if err := upsertKnownHost(releasedUser.ID, ident, host); err != nil {
			loggerUsers.Warn("failed to upsert known host", "error", err)
		}
		return releasedUser, nil
	}

	return createNewUser(network, nick, norm, "", ident, host)
}

// resolveUserFallback is invoked when resolveUser's normal flow fails with a
// UNIQUE-constraint violation that retries could not avoid. It creates a new
// flagged user row. With the partial unique index in place, flagged rows are
// excluded from uniqueness so we can write the real normalized_nick without
// needing a sentinel payload.
//
// Logs at ERROR with a distinctive message so admins can grep / alert.
//
// On insert failure, returns ErrUserResolveCollision so callers can surface
// a persistent notice and stop. Does NOT loop or retry the fallback itself.
func resolveUserFallback(network, nick, ident, host, account, casemapping string, cause error) (*User, error) {
	norm := normalizeIRC(nick, casemapping)
	now := time.Now()
	user := &User{
		Network:        network,
		CurrentNick:    nick,
		NormalizedNick: norm,
		IRCAccount:     account,
		Flagged:        true,
		FlaggedReason:  FlaggedReasonCollision,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := theDB.Create(user).Error; err != nil {
		loggerUsers.Error("flagged_user_create_failed",
			"network", network, "nick", nick, "account", account,
			"cause", cause.Error(), "fallback_error", err.Error())
		return nil, &ErrUserResolveCollision{
			Network: network,
			Nick:    nick,
			Account: account,
			Err:     cause,
		}
	}
	if err := upsertKnownHost(user.ID, ident, host); err != nil {
		loggerUsers.Warn("failed to upsert known host", "error", err)
	}
	loggerUsers.Error("flagged_user_created_admin_attention_required",
		"user_id", user.ID,
		"network", network,
		"nick", nick,
		"account", account,
		"reason", user.FlaggedReason,
		"cause", cause.Error())
	return user, nil
}

// resolveUserByNick looks up a user by their current nick. Used by LLM ban
// tool and TUI commands which only see nicks, not ident/host. Skips
// released and flagged rows so the lookup behaves as if those rows don't
// hold the nick.
func resolveUserByNick(network, nick, casemapping string) (*User, error) {
	if theDB == nil {
		return nil, nil
	}
	return getActiveUserByNormalizedNick(network, normalizeIRC(nick, casemapping))
}

// getUserByAccount returns the user with the given network + IRC services
// account. Flagged rows are excluded so they cannot be matched as the
// canonical identity for that account — they are diagnostic placeholders
// awaiting admin cleanup.
//
// Released rows ARE returned: if someone with an account quits and comes
// back, we want to re-attach them to the existing row (which has their
// known hosts, bans, etc.). resolveUserOnce sets Released=false on match.
func getUserByAccount(network, account string) (*User, error) {
	if account == "" {
		return nil, nil
	}
	var user User
	err := theDB.Where("network = ? AND account = ? AND flagged = ?", network, account, false).First(&user).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// getActiveUserByNormalizedNick returns the *active* user (not released,
// not flagged) holding the given normalized_nick on the network. This is
// what callers want when checking "who owns this nick right now". With the
// partial unique index there can only ever be at most one such row.
func getActiveUserByNormalizedNick(network, normalizedNick string) (*User, error) {
	var user User
	err := theDB.Where(
		"network = ? AND normalized_nick = ? AND released = ? AND flagged = ?",
		network, normalizedNick, false, false,
	).First(&user).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

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
// If more than one released row matches, returns the newest by updated_at
// and the total match count so the caller can WARN about ambiguity. This
// happens after multiple release/reclaim cycles without account or host
// evidence linking them together — the bot is making a best-effort guess.
//
// Returns (nil, 0, nil) when there are no matches.
//
// Security note: on zero-trust networks this extends nick continuity across
// disconnects, which means anyone re-using a released nick inherits the
// previous owner's identity (sessions, bans, history). This is the same
// trust posture the bot already had via in-channel nick continuity — the
// fallback just preserves it across QUIT/PART/KICK. Full mitigation is
// deferred to the Phase 5 account system.
func getMostRecentReleasedUserByNormalizedNick(network, normalizedNick string) (*User, int64, error) {
	var count int64
	err := theDB.Model(&User{}).Where(
		"network = ? AND normalized_nick = ? AND released = ? AND flagged = ?",
		network, normalizedNick, true, false,
	).Count(&count).Error
	if err != nil {
		return nil, 0, err
	}
	if count == 0 {
		return nil, 0, nil
	}
	var user User
	err = theDB.Where(
		"network = ? AND normalized_nick = ? AND released = ? AND flagged = ?",
		network, normalizedNick, true, false,
	).Order("updated_at DESC, id DESC").First(&user).Error
	if err != nil {
		return nil, count, err
	}
	return &user, count, nil
}

// recoverByKnownHost attempts to re-associate a user via ident@host when the
// nick is not recognized (bot restart scenario). If ident@host matches
// multiple users, cross-references the normalized nick against nick_changes
// history to disambiguate. Returns nil if no match or ambiguous.
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
func recoverByKnownHost(network, ident, host, normalizedNick string) (*User, error) {
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
	if len(hosts) == 1 {
		var user User
		if err := theDB.First(&user, hosts[0].UserID).Error; err != nil {
			return nil, err
		}
		loggerUsers.Debug("host recovery: single match", "user_id", user.ID, "ident", ident, "host", host, "nick", normalizedNick, "network", network)
		return &user, nil
	}

	loggerUsers.Debug("host recovery: multiple matches, disambiguating via nick_changes", "count", len(hosts), "ident", ident, "host", host, "nick", normalizedNick, "network", network)
	for _, h := range hosts {
		var count int64
		theDB.Model(&NickChange{}).
			Where("user_id = ? AND (normalized_old = ? OR normalized_new = ?)",
				h.UserID, normalizedNick, normalizedNick).
			Count(&count)
		if count > 0 {
			var user User
			if err := theDB.First(&user, h.UserID).Error; err != nil {
				return nil, err
			}
			loggerUsers.Debug("host recovery: disambiguated via nick_changes", "user_id", user.ID, "nick_changes_count", count, "network", network)
			return &user, nil
		}
	}

	loggerUsers.Debug("host recovery: ambiguous, no nick_change match for any candidate", "ident", ident, "host", host, "nick", normalizedNick, "network", network)
	return nil, nil
}

func createNewUser(network, nick, normalizedNick, account, ident, host string) (*User, error) {
	now := time.Now()
	user := User{
		Network:        network,
		CurrentNick:    nick,
		NormalizedNick: normalizedNick,
		IRCAccount:     account,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := theDB.Create(&user).Error; err != nil {
		return nil, err
	}
	if err := upsertKnownHost(user.ID, ident, host); err != nil {
		loggerUsers.Warn("failed to upsert known host", "error", err)
	}
	loggerUsers.Info("created new user", "id", user.ID, "network", network, "nick", nick)
	return &user, nil
}

func updateDBUser(user *User) error {
	user.UpdatedAt = time.Now()
	return theDB.Save(user).Error
}

func upsertKnownHost(userID int64, ident, host string) error {
	if ident == "" || host == "" {
		return nil
	}
	now := time.Now()
	var existing UserKnownHost
	err := theDB.Where("user_id = ? AND ident = ? AND host = ?",
		userID, ident, host).First(&existing).Error
	if err == nil {
		return theDB.Model(&existing).Update("last_seen", now).Error
	}
	if err == gorm.ErrRecordNotFound {
		loggerUsers.Debug("new host for user", "user_id", userID, "ident", ident, "host", host)
		return theDB.Create(&UserKnownHost{
			UserID:    userID,
			Ident:     ident,
			Host:      host,
			FirstSeen: now,
			LastSeen:  now,
		}).Error
	}
	return err
}

// releaseUserNick marks a user's nick claim as released so another user can
// take it over. The user's current_nick and normalized_nick are preserved
// (the partial unique index idx_users_nick_active excludes released rows,
// so this row no longer blocks another active user from claiming the same
// nick). The row stays in place for host-based recovery on reconnect, and
// for display in UIs (admins searching by old nicks, etc.).
func releaseUserNick(userID int64) error {
	return theDB.Model(&User{}).Where("id = ?", userID).
		Updates(map[string]interface{}{
			"released":   true,
			"updated_at": time.Now(),
		}).Error
}

// hasNoKnownHosts returns true if the user has zero entries in user_known_hosts.
// Migration-era users created by createUsersFromSessions have no host history
// — they are safe merge candidates during nick collision resolution.
func hasNoKnownHosts(userID int64) (bool, error) {
	var count int64
	if err := theDB.Model(&UserKnownHost{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		return false, fmt.Errorf("checking known hosts for user %d: %w", userID, err)
	}
	return count == 0, nil
}

// mergeUser reassigns all data from ghostUserID to targetUserID, then deletes
// the ghost user. Used when a migration-era user (no known hosts) is displaced
// by a real user taking their nick. All sessions, bans, nick changes, etc. are
// consolidated under the surviving user.
func mergeUser(ghostUserID, targetUserID int64) error {
	loggerUsers.Info("merging ghost user into target", "ghost_user_id", ghostUserID, "target_user_id", targetUserID)

	tables := []struct {
		tableName string
		model     interface{}
		column    string
	}{
		{"sessions", &Session{}, "user_id"},
		{"pending_jobs", &PendingJob{}, "user_id"},
		{"nick_changes", &NickChange{}, "user_id"},
		{"bans", &Ban{}, "user_id"},
		{"bans", &Ban{}, "banner_user_id"},
		{"user_known_hosts", &UserKnownHost{}, "user_id"},
	}

	err := theDB.Transaction(func(tx *gorm.DB) error {
		for _, tbl := range tables {
			result := tx.Model(tbl.model).
				Where(fmt.Sprintf("%s = ?", tbl.column), ghostUserID).
				Update(tbl.column, targetUserID)
			if result.Error != nil {
				return fmt.Errorf("merging %s.%s: %w", tbl.tableName, tbl.column, result.Error)
			}
			if result.RowsAffected > 0 {
				loggerUsers.Debug("reassigned rows", "table", tbl.tableName, "column", tbl.column, "count", result.RowsAffected)
			}
		}

		if err := tx.Delete(&User{}, ghostUserID).Error; err != nil {
			return fmt.Errorf("deleting ghost user %d: %w", ghostUserID, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	loggerUsers.Info("merged ghost user", "ghost_user_id", ghostUserID, "target_user_id", targetUserID)
	return nil
}

// handleNickCollision is called when a nick change would collide with an
// existing user's normalized_nick. If the existing user has no known hosts
// (migration-era ghost), they are merged into the changing user. Otherwise,
// the existing user's nick is released (they're offline/displaced).
//
// Note on involuntary release: this reuses releaseUserNick to free the
// (network, normalized_nick) slot for the new owner, even though the
// displaced user did not actually quit. The displaced row is then eligible
// for the released-nick fallback in resolveUserOnce on a future rejoin,
// just like a voluntarily-released row. On accountless networks this means
// a stranger arriving with the displaced nick from a fresh host can be
// re-attached to the displaced row, inheriting its sessions / bans /
// history.
//
// The precondition (two active rows on the same normalized_nick) is hard
// to reach on real IRC servers: the server enforces nick uniqueness, so
// realistic sources are netsplit corner cases or DB-state drift, not
// anything a user can deliberately trigger. Full mitigation (e.g. a
// separate Displaced flag excluded from the nick fallback) is deferred to
// the Phase 5 account system which resolves identity more rigorously.
func handleNickCollision(network string, existingUser *User, changingUser *User) error {
	noHosts, err := hasNoKnownHosts(existingUser.ID)
	if err != nil {
		return err
	}
	if noHosts {
		loggerUsers.Info("nick collision with ghost user, merging",
			"ghost_user_id", existingUser.ID, "ghost_nick", existingUser.CurrentNick,
			"surviving_user_id", changingUser.ID, "surviving_nick", changingUser.CurrentNick,
			"network", network)
		return mergeUser(existingUser.ID, changingUser.ID)
	}

	loggerUsers.Info("nick collision with user who has host history, releasing their nick",
		"released_user_id", existingUser.ID, "released_nick", existingUser.CurrentNick,
		"taking_user_id", changingUser.ID, "taking_nick", changingUser.CurrentNick,
		"network", network)
	return releaseUserNick(existingUser.ID)
}

// recordNickChange logs a nick change for a tracked user. Returns true if a
// user was found and updated. Handles nick collisions: if another user already
// holds the target nick, the existing user is either merged (if they're a
// migration-era ghost with no known hosts) or displaced (their nick is
// released for host-based recovery on reconnect).
func recordNickChange(network, oldNick, newNick, casemapping string) bool {
	if theDB == nil {
		return false
	}
	normOld := normalizeIRC(oldNick, casemapping)
	normNew := normalizeIRC(newNick, casemapping)

	user, err := getActiveUserByNormalizedNick(network, normOld)
	if err != nil || user == nil {
		loggerUsers.Debug("nick change: old nick not tracked", "old", oldNick, "new", newNick, "network", network)
		return false
	}

	existing, err := getActiveUserByNormalizedNick(network, normNew)
	if err != nil {
		loggerUsers.Error("nick change: error checking collision", "error", err)
		return false
	}
	if existing != nil && existing.ID != user.ID {
		if err := handleNickCollision(network, existing, user); err != nil {
			loggerUsers.Error("nick change: failed to handle collision", "error", err)
			return false
		}
	}

	user.CurrentNick = newNick
	user.NormalizedNick = normNew
	if err := updateDBUser(user); err != nil {
		loggerUsers.Error("failed to update user nick", "error", err)
		return false
	}

	theDB.Create(&NickChange{
		UserID:        user.ID,
		OldNick:       oldNick,
		NewNick:       newNick,
		NormalizedOld: normOld,
		NormalizedNew: normNew,
		CreatedAt:     time.Now(),
	})
	return true
}

// getUserByID returns a user by their primary key.
func getUserByID(id int64) (*User, error) {
	var user User
	if err := theDB.First(&user, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// getFlaggedUsers returns up to 50 flagged rows ordered by created_at desc.
// If network is non-empty, results are restricted to that network. Used by
// the TUI /flagged command and admin diagnostics.
func getFlaggedUsers(network string) ([]User, error) {
	if theDB == nil {
		return nil, nil
	}
	var users []User
	q := theDB.Where("flagged = ?", true)
	if network != "" {
		q = q.Where("network = ?", network)
	}
	if err := q.Order("created_at DESC").Limit(50).Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// countFlaggedUsers returns the total number of flagged rows across all
// networks. Used by the TUI status bar to surface "flagged:N" so admins
// notice when resolveUser fell back to placeholder rows.
func countFlaggedUsers() (int64, error) {
	if theDB == nil {
		return 0, nil
	}
	var n int64
	if err := theDB.Model(&User{}).Where("flagged = ?", true).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

type UserInfo struct {
	User         User
	Hosts        []UserKnownHost
	SessionCount int
	MessageCount int
	ActiveBans   []Ban
	NickChanges  []NickChange
}

func getUserInfo(userID int64) (*UserInfo, error) {
	user, err := getUserByID(userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, nil
	}

	info := &UserInfo{User: *user}

	theDB.Where("user_id = ?", userID).Order("last_seen DESC").Find(&info.Hosts)

	info.SessionCount, info.MessageCount, err = getUserDBStatsAllNetworks(userID)
	if err != nil {
		return nil, fmt.Errorf("getting user stats: %w", err)
	}

	now := time.Now()
	theDB.Where("user_id = ? AND active = ? AND expires_at > ?", userID, true, now).
		Order("created_at DESC").Find(&info.ActiveBans)

	theDB.Where("user_id = ?", userID).Order("created_at DESC").Limit(20).Find(&info.NickChanges)

	return info, nil
}

func getUserDBStatsAllNetworks(userID int64) (int, int, error) {
	var sessionCount int64
	err := theDB.Model(&Session{}).Where("user_id = ?", userID).Count(&sessionCount).Error
	if err != nil {
		return 0, 0, err
	}
	var sessionIDs []int64
	err = theDB.Model(&Session{}).Where("user_id = ?", userID).Pluck("id", &sessionIDs).Error
	if err != nil {
		return int(sessionCount), 0, err
	}
	if len(sessionIDs) == 0 {
		return int(sessionCount), 0, nil
	}
	var messageCount int64
	err = theDB.Model(&Message{}).Where("session_id IN ?", sessionIDs).Count(&messageCount).Error
	return int(sessionCount), int(messageCount), err
}

type UserSearchResult struct {
	ID             int64
	CurrentNick    string
	NormalizedNick string
	IRCAccount     string
	HostCount      int
	SessionCount   int
	Released       bool
}

// DisplayName returns the display nick for a search result. Mirrors
// displayNick(); kept as a method so callers can use r.DisplayName() in
// templates and format strings without dereferencing.
func (r *UserSearchResult) DisplayName() string {
	return r.CurrentNick
}

func searchUsers(network, query string) ([]UserSearchResult, error) {
	var users []User

	if query == "*" {
		if err := theDB.Where("network = ?", network).
			Order("id ASC").Limit(50).Find(&users).Error; err != nil {
			return nil, err
		}
		return buildSearchResults(users), nil
	}

	isNum := false
	if _, err := strconv.ParseInt(query, 10, 64); err == nil {
		isNum = true
	}

	var pattern string
	if strings.Contains(query, "*") {
		pattern = strings.ToLower(strings.ReplaceAll(query, "*", "%"))
	} else {
		pattern = "%" + strings.ToLower(query) + "%"
	}

	db := theDB.Where("network = ?", network)

	if isNum {
		id, _ := strconv.ParseInt(query, 10, 64)
		db = db.Where("id = ? OR LOWER(current_nick) LIKE ? OR LOWER(account) LIKE ?",
			id, pattern, pattern)
	} else {
		db = db.Where("LOWER(current_nick) LIKE ? OR LOWER(account) LIKE ?",
			pattern, pattern)
	}

	if err := db.Order("id ASC").Limit(50).Find(&users).Error; err != nil {
		return nil, err
	}

	var hostUsers []int64
	theDB.Model(&UserKnownHost{}).
		Where("LOWER(ident) LIKE ? OR LOWER(host) LIKE ?", pattern, pattern).
		Pluck("DISTINCT user_id", &hostUsers)

	if len(hostUsers) > 0 {
		var hostMatchedUsers []User
		theDB.Where("network = ? AND id IN ?", network, hostUsers).
			Order("id ASC").Limit(50).Find(&hostMatchedUsers)
		users = append(users, hostMatchedUsers...)
	}

	return buildSearchResults(users), nil
}

func buildSearchResults(users []User) []UserSearchResult {
	seen := make(map[int64]bool)
	var results []UserSearchResult
	for _, u := range users {
		if seen[u.ID] {
			continue
		}
		seen[u.ID] = true

		var hostCount int64
		theDB.Model(&UserKnownHost{}).Where("user_id = ?", u.ID).Count(&hostCount)

		var sessionCount int64
		theDB.Model(&Session{}).Where("user_id = ?", u.ID).Count(&sessionCount)

		results = append(results, UserSearchResult{
			ID:             u.ID,
			CurrentNick:    u.CurrentNick,
			NormalizedNick: u.NormalizedNick,
			IRCAccount:     u.IRCAccount,
			HostCount:      int(hostCount),
			SessionCount:   int(sessionCount),
			Released:       u.Released,
		})
	}
	return results
}

func computeMergeHash(ghost, target *User) string {
	data := fmt.Sprintf("%d:%s:%d:%s", ghost.ID, ghost.CurrentNick, target.ID, target.CurrentNick)
	hash := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", hash)[:8]
}
