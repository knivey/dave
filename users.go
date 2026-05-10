package main

import (
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"gorm.io/gorm"
)

var loggerUsers = logxi.New("users")

func init() {
	loggerUsers.SetLevel(logxi.LevelAll)
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

	norm := normalizeIRC(nick, casemapping)

	if account != "" {
		user, err := getUserByAccount(network, account)
		if err != nil {
			return nil, err
		}
		if user != nil {
			loggerUsers.Debug("resolved user", "method", "account", "user_id", user.ID, "nick", nick, "account", account, "network", network)
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

// resolveUserByNick looks up a user by their current nick. Used by LLM ban
// tool and TUI commands which only see nicks, not ident/host.
func resolveUserByNick(network, nick, casemapping string) (*User, error) {
	if theDB == nil {
		return nil, nil
	}
	return getUserByNormalizedNick(network, normalizeIRC(nick, casemapping))
}

func getUserByAccount(network, account string) (*User, error) {
	if account == "" {
		return nil, nil
	}
	var user User
	err := theDB.Where("network = ? AND account = ?", network, account).First(&user).Error
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
func recoverByKnownHost(network, ident, host, normalizedNick string) (*User, error) {
	var hosts []UserKnownHost
	err := theDB.Joins("JOIN users ON users.id = user_known_hosts.user_id").
		Where("users.network = ? AND user_known_hosts.ident = ? AND user_known_hosts.host = ?",
			network, ident, host).
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

// recordNickChange logs a nick change for a tracked user. Returns true if a
// user was found and updated.
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
