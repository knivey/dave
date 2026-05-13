package main

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"gorm.io/gorm"
)

const releasedNickPrefix = ",quit,"
const flaggedNickPrefix = ",flagged,"

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

// flaggedSeq is a process-local monotonic counter appended to flagged-row
// sentinels. UnixNano alone would have a vanishing collision window on
// systems with coarse clock resolution or in tight concurrent bursts; the
// counter makes the sentinel collision-free regardless of clock behavior.
var flaggedSeq atomic.Int64

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

// isFlaggedNick returns true if the normalized nick is a flagged-row sentinel
// (created when resolveUser fell back to creating a placeholder user row).
func isFlaggedNick(s string) bool {
	return strings.HasPrefix(s, flaggedNickPrefix)
}

// isPlaceholderNick returns true for any non-claimable normalized_nick: both
// released sentinels (",quit,<id>") and flagged sentinels (",flagged,...").
// resolveUser and resolveUserByNick skip rows matching this so the real nick
// remains available for the legitimate owner.
func isPlaceholderNick(s string) bool {
	return isReleasedNick(s) || isFlaggedNick(s)
}

var loggerUsers = logxi.New("users")

func init() {
	loggerUsers.SetLevel(logxi.LevelAll)
}

// claimNickFor ensures `user` may safely take `(network, norm)` as its
// normalized_nick before the caller assigns it and writes the row. If a
// different user currently holds that slot, handleNickCollision either merges
// them into `user` (when they have no known hosts — migration-era ghost) or
// releases their nick to a `,quit,<id>` sentinel (real user, presumed
// offline/displaced). After this returns nil, the (network, norm) pair is
// guaranteed free for `user` to claim.
//
// This mirrors the collision handling recordNickChange performs for NICK
// events. resolveUser needs the same guard because it can identify a user by
// account or by ident@host recovery and then try to assign that user a
// normalized_nick already owned by another row, violating the UNIQUE index
// idx_users_nick on (network, normalized_nick).
func claimNickFor(network string, user *User, norm string) error {
	if user.NormalizedNick == norm {
		return nil
	}
	existing, err := getUserByNormalizedNick(network, norm)
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
			user.CurrentNick = nick
			user.NormalizedNick = norm
			if err := updateDBUser(user); err != nil {
				return nil, err
			}
			_ = upsertKnownHost(user.ID, ident, host)
			return user, nil
		}

		loggerUsers.Debug("account lookup missed, trying nick lookup", "account", account, "nick", nick, "network", network)
		nickUser, err := getUserByNormalizedNick(network, norm)
		if err != nil {
			return nil, err
		}
		if nickUser != nil && isPlaceholderNick(nickUser.NormalizedNick) {
			loggerUsers.Debug("nick lookup hit placeholder user, skipping", "user_id", nickUser.ID, "nick", nick, "network", network)
			nickUser = nil
		}
		if nickUser != nil {
			loggerUsers.Debug("resolved user", "method", "nick+account_link", "user_id", nickUser.ID, "nick", nick, "account", account, "network", network)
			nickUser.IRCAccount = account
			nickUser.CurrentNick = nick
			nickUser.NormalizedNick = norm
			if err := updateDBUser(nickUser); err != nil {
				return nil, err
			}
			_ = upsertKnownHost(nickUser.ID, ident, host)
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
			hostUser.IRCAccount = account
			hostUser.CurrentNick = nick
			hostUser.NormalizedNick = norm
			if err := updateDBUser(hostUser); err != nil {
				return nil, err
			}
			_ = upsertKnownHost(hostUser.ID, ident, host)
			return hostUser, nil
		}

		return createNewUser(network, nick, norm, account, ident, host)
	}

	user, err := getUserByNormalizedNick(network, norm)
	if err != nil {
		return nil, err
	}
	if user != nil {
		if isPlaceholderNick(user.NormalizedNick) {
			loggerUsers.Debug("nick lookup hit placeholder user, falling through to host recovery", "user_id", user.ID, "nick", nick, "network", network)
			user = nil
		}
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
		_ = upsertKnownHost(user.ID, ident, host)
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
		if hostUser.IRCAccount == "" && account != "" {
			hostUser.IRCAccount = account
		}
		hostUser.CurrentNick = nick
		hostUser.NormalizedNick = norm
		if err := updateDBUser(hostUser); err != nil {
			return nil, err
		}
		_ = upsertKnownHost(hostUser.ID, ident, host)
		return hostUser, nil
	}

	return createNewUser(network, nick, norm, "", ident, host)
}

// resolveUserFallback is invoked when resolveUser's normal flow fails with a
// UNIQUE-constraint violation that retries could not avoid. It creates a new
// flagged user row with a sentinel normalized_nick that cannot collide with
// any real IRC nick (leading comma reserved). The caller sees a valid *User
// and the message keeps flowing.
//
// Logs at ERROR with a distinctive message so admins can grep / alert.
//
// On insert failure, returns ErrUserResolveCollision so callers can surface
// a persistent notice and stop. Does NOT loop or retry the fallback itself.
func resolveUserFallback(network, nick, ident, host, account, casemapping string, cause error) (*User, error) {
	norm := normalizeIRC(nick, casemapping)
	sentinel := fmt.Sprintf("%s%s,%s,%d,%d",
		flaggedNickPrefix, network, norm,
		time.Now().UnixNano(),
		flaggedSeq.Add(1))
	now := time.Now()
	user := &User{
		Network:        network,
		CurrentNick:    nick,
		NormalizedNick: sentinel,
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
	_ = upsertKnownHost(user.ID, ident, host)
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
// placeholder rows (released or flagged) so the lookup behaves as if those
// rows don't hold the nick.
func resolveUserByNick(network, nick, casemapping string) (*User, error) {
	if theDB == nil {
		return nil, nil
	}
	user, err := getUserByNormalizedNick(network, normalizeIRC(nick, casemapping))
	if err != nil {
		return nil, err
	}
	if user != nil && isPlaceholderNick(user.NormalizedNick) {
		return nil, nil
	}
	return user, nil
}

// getUserByAccount returns the (non-flagged) user with the given network +
// IRC services account. Flagged rows are excluded so they cannot be matched
// as the canonical identity for that account — they are diagnostic
// placeholders awaiting admin cleanup.
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

func getUserByNormalizedNick(network, normalizedNick string) (*User, error) {
	var user User
	err := theDB.Where("network = ? AND normalized_nick = ?", network, normalizedNick).First(&user).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
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
	_ = upsertKnownHost(user.ID, ident, host)
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

// isReleasedNick returns true if the normalized nick is a released sentinel
// value (set when a user quits or is displaced by a nick collision).
func isReleasedNick(normalizedNick string) bool {
	return strings.HasPrefix(normalizedNick, releasedNickPrefix)
}

// releaseUserNick clears a user's nick claim so another user can take it over.
// Sets normalized_nick to a unique sentinel that cannot collide with real IRC
// nicks (commas are not valid in IRC nicknames). The user record is preserved
// for host-based recovery on reconnect.
func releaseUserNick(userID int64) error {
	sentinel := fmt.Sprintf("%s%d", releasedNickPrefix, userID)
	return theDB.Model(&User{}).Where("id = ?", userID).
		Updates(map[string]interface{}{
			"normalized_nick": sentinel,
			"current_nick":    "",
			"updated_at":      time.Now(),
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

	user, err := getUserByNormalizedNick(network, normOld)
	if err != nil || user == nil {
		loggerUsers.Debug("nick change: old nick not tracked", "old", oldNick, "new", newNick, "network", network)
		return false
	}

	existing, err := getUserByNormalizedNick(network, normNew)
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
			Released:       isReleasedNick(u.NormalizedNick),
		})
	}
	return results
}

func computeMergeHash(ghost, target *User) string {
	data := fmt.Sprintf("%d:%s:%d:%s", ghost.ID, ghost.CurrentNick, target.ID, target.CurrentNick)
	hash := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", hash)[:8]
}
