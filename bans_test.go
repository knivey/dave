package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateBan(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")
	duration := 1 * time.Hour

	ban, err := createBan(theDB, userID, "testnet", "#test", "", "spamming", duration, nil, "admin")
	require.NoError(t, err)
	require.NotNil(t, ban)

	assert.NotZero(t, ban.ID)
	assert.Equal(t, userID, ban.UserID)
	assert.Equal(t, "testnet", ban.Network)
	assert.Equal(t, "#test", ban.Channel)
	assert.Equal(t, "", ban.ServiceScope)
	assert.Equal(t, "spamming", ban.Reason)
	assert.Equal(t, duration, ban.Duration)
	assert.True(t, ban.Active)
	assert.Nil(t, ban.BannerUserID)
	assert.Equal(t, "admin", ban.BannerNick)
	assert.False(t, ban.ExpiresAt.IsZero())
	assert.WithinDuration(t, time.Now().Add(duration), ban.ExpiresAt, 5*time.Second)
}

func TestCreateBanWithBanner(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")
	bannerID := ensureTestUser(t, "testnet", "Admin")

	ban, err := createBan(theDB, userID, "testnet", "#test", "", "abuse", 30*time.Minute, &bannerID, "Admin")
	require.NoError(t, err)
	require.NotNil(t, ban)
	assert.Equal(t, &bannerID, ban.BannerUserID)
}

func TestIsBannedActive(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	_, err := createBan(theDB, userID, "testnet", "", "", "spamming", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	assert.True(t, isBanned(theDB, userID, "testnet", "#test", ""))
	assert.True(t, isBanned(theDB, userID, "testnet", "#other", ""), "empty channel ban matches any channel")
}

func TestIsBannedChannelSpecific(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	_, err := createBan(theDB, userID, "testnet", "#test", "", "spamming", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	assert.True(t, isBanned(theDB, userID, "testnet", "#test", ""))
	assert.False(t, isBanned(theDB, userID, "testnet", "#other", ""), "channel-specific ban does not match other channel")
}

func TestIsBannedDifferentNetwork(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	_, err := createBan(theDB, userID, "testnet", "#test", "", "spamming", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	assert.False(t, isBanned(theDB, userID, "othernet", "#test", ""))
}

func TestIsBannedExpiredLazyDeactivation(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	ban, err := createBan(theDB, userID, "testnet", "", "", "spamming", 1*time.Nanosecond, nil, "admin")
	require.NoError(t, err)
	require.NotNil(t, ban)

	time.Sleep(10 * time.Millisecond)

	assert.False(t, isBanned(theDB, userID, "testnet", "#test", ""), "expired ban should not count")

	var updated Ban
	theDB.First(&updated, ban.ID)
	assert.False(t, updated.Active, "expired ban should be lazily deactivated")
}

func TestIsBannedNilDB(t *testing.T) {
	assert.False(t, isBanned(nil, 1, "testnet", "#test", ""))
}

func TestDeactivateBan(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	ban, err := createBan(theDB, userID, "testnet", "", "", "spamming", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	err = deactivateBan(theDB, ban.ID)
	require.NoError(t, err)

	assert.False(t, isBanned(theDB, userID, "testnet", "#test", ""))

	var updated Ban
	theDB.First(&updated, ban.ID)
	assert.False(t, updated.Active)
	assert.NotNil(t, updated.DeactivatedAt)
}

func TestDeactivateBansForUser(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	_, err := createBan(theDB, userID, "testnet", "#test", "", "spam", 1*time.Hour, nil, "admin")
	require.NoError(t, err)
	_, err = createBan(theDB, userID, "testnet", "#other", "", "abuse", 2*time.Hour, nil, "admin")
	require.NoError(t, err)

	err = deactivateBansForUser(theDB, userID, "testnet")
	require.NoError(t, err)

	assert.False(t, isBanned(theDB, userID, "testnet", "#test", ""))
	assert.False(t, isBanned(theDB, userID, "testnet", "#other", ""))
}

func TestDeactivateBansForUserNetworkSpecific(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	_, err := createBan(theDB, userID, "testnet", "", "", "spam", 1*time.Hour, nil, "admin")
	require.NoError(t, err)
	_, err = createBan(theDB, userID, "othernet", "", "", "spam", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	err = deactivateBansForUser(theDB, userID, "testnet")
	require.NoError(t, err)

	assert.False(t, isBanned(theDB, userID, "testnet", "#test", ""))
	assert.True(t, isBanned(theDB, userID, "othernet", "#test", ""))
}

func TestGetActiveBans(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user1 := ensureTestUser(t, "testnet", "User1")
	user2 := ensureTestUser(t, "testnet", "User2")

	_, err := createBan(theDB, user1, "testnet", "#test", "", "spam", 1*time.Hour, nil, "admin")
	require.NoError(t, err)
	_, err = createBan(theDB, user2, "testnet", "#test", "", "abuse", 2*time.Hour, nil, "admin")
	require.NoError(t, err)

	bans, err := getActiveBans(theDB, "testnet")
	require.NoError(t, err)
	assert.Len(t, bans, 2)

	bans, err = getActiveBans(theDB, "othernet")
	require.NoError(t, err)
	assert.Len(t, bans, 0)
}

func TestGetActiveBansExcludesInactive(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	ban, err := createBan(theDB, userID, "testnet", "", "", "spam", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	err = deactivateBan(theDB, ban.ID)
	require.NoError(t, err)

	bans, err := getActiveBans(theDB, "testnet")
	require.NoError(t, err)
	assert.Len(t, bans, 0)
}

func TestGetBanHistory(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	_, err := createBan(theDB, userID, "testnet", "", "", "spam", 1*time.Hour, nil, "admin")
	require.NoError(t, err)
	_, err = createBan(theDB, userID, "testnet", "#test", "", "abuse", 2*time.Hour, nil, "admin")
	require.NoError(t, err)

	history, err := getBanHistory(theDB, userID)
	require.NoError(t, err)
	assert.Len(t, history, 2)
}

func TestGetBanHistoryEmpty(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "GoodUser")

	history, err := getBanHistory(theDB, userID)
	require.NoError(t, err)
	assert.Len(t, history, 0)
}

func TestSweepExpiredBans(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user1 := ensureTestUser(t, "testnet", "User1")
	user2 := ensureTestUser(t, "testnet", "User2")

	expired, err := createBan(theDB, user1, "testnet", "", "", "spam", 1*time.Nanosecond, nil, "admin")
	require.NoError(t, err)
	_ = expired

	active, err := createBan(theDB, user2, "testnet", "", "", "abuse", 1*time.Hour, nil, "admin")
	require.NoError(t, err)
	_ = active

	time.Sleep(10 * time.Millisecond)

	sweepExpiredBans(theDB)

	var count int64
	theDB.Model(&Ban{}).Where("active = ?", true).Count(&count)
	assert.Equal(t, int64(1), count)
}

func TestSweepExpiredBansNilDB(t *testing.T) {
	sweepExpiredBans(nil)
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input    time.Duration
		expected string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{30 * time.Minute, "30m"},
		{1 * time.Hour, "1h0m"},
		{90 * time.Minute, "1h30m"},
		{24 * time.Hour, "1d0h"},
		{36 * time.Hour, "1d12h"},
		{7*24*time.Hour + 2*time.Hour, "7d2h"},
	}

	for _, tc := range tests {
		t.Run(tc.input.String(), func(t *testing.T) {
			assert.Equal(t, tc.expected, formatDuration(tc.input))
		})
	}
}

func TestParseBanDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
		wantErr  bool
	}{
		{"5m", 5 * time.Minute, false},
		{"1h", 1 * time.Hour, false},
		{"2h30m", 2*time.Hour + 30*time.Minute, false},
		{"30s", 30 * time.Second, false},
		{"1d", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"0d", 0, false},
		{"7d2h", 0, true},
		{"abc", 0, true},
		{"-1d", 0, true},
		{"", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseBanDuration(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestIsBannedServiceScope(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	_, err := createBan(theDB, userID, "testnet", "#test", "img", "image spam", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	assert.True(t, isBanned(theDB, userID, "testnet", "#test", "img"), "matching service scope")
	assert.False(t, isBanned(theDB, userID, "testnet", "#test", "chat"), "non-matching service scope")
	assert.False(t, isBanned(theDB, userID, "testnet", "#test", ""), "empty service scope does not match specific scope")
}

func TestMultipleBansOneActive(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "BadUser")

	ban1, err := createBan(theDB, userID, "testnet", "", "", "first offense", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	err = deactivateBan(theDB, ban1.ID)
	require.NoError(t, err)

	_, err = createBan(theDB, userID, "testnet", "", "", "second offense", 2*time.Hour, nil, "admin")
	require.NoError(t, err)

	assert.True(t, isBanned(theDB, userID, "testnet", "#test", ""))

	history, err := getBanHistory(theDB, userID)
	require.NoError(t, err)
	assert.Len(t, history, 2)
}
