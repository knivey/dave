package main

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var ensureTestUserMu sync.Mutex

func ensureTestUser(t *testing.T, network, nick string) int64 {
	t.Helper()
	ensureTestUserMu.Lock()
	defer ensureTestUserMu.Unlock()
	var user User
	err := theDB.Where("network = ? AND normalized_nick = ?", network, normalizeIRC(nick, "rfc1459")).First(&user).Error
	if err == nil {
		return user.ID
	}
	user = User{
		Network:        network,
		CurrentNick:    nick,
		NormalizedNick: normalizeIRC(nick, "rfc1459"),
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	require.NoError(t, theDB.Create(&user).Error, "create test user")
	return user.ID
}
