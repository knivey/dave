package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	logxi "github.com/mgutz/logxi/v1"
)

func setupUserTestDB(t *testing.T) func() {
	t.Helper()
	db, err := initDB(DatabaseConfig{Path: t.TempDir() + "/test.db"}, logxi.New("test"))
	require.NoError(t, err, "initDB")
	oldDB := theDB
	theDB = db
	return func() {
		closeDB(theDB)
		theDB = oldDB
	}
}

func TestResolveUserCreatesNew(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestNick", "ident1", "host1.example.com", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	assert.Equal(t, "testnet", user.Network)
	assert.Equal(t, "TestNick", user.CurrentNick)
	assert.Equal(t, "testnick", user.NormalizedNick)
	assert.Equal(t, "", user.IRCAccount)
	assert.NotZero(t, user.ID)

	var hosts []UserKnownHost
	theDB.Where("user_id = ?", user.ID).Find(&hosts)
	require.Len(t, hosts, 1)
	assert.Equal(t, "ident1", hosts[0].Ident)
	assert.Equal(t, "host1.example.com", hosts[0].Host)
}

func TestResolveUserByAccount(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "Nick1", "ident1", "host1", "myaccount", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "myaccount", user.IRCAccount)

	user2, err := resolveUser("testnet", "Nick2", "ident1", "host1", "myaccount", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user2)
	assert.Equal(t, user.ID, user2.ID)
	assert.Equal(t, "Nick2", user2.CurrentNick)
	assert.Equal(t, "nick2", user2.NormalizedNick)
}

func TestResolveUserByNickContinuity(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "MyNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	user2, err := resolveUser("testnet", "mynick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user2)
	assert.Equal(t, user.ID, user2.ID)
}

func TestResolveUserNickCasingUpdate(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	user2, err := resolveUser("testnet", "TESTNICK", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user2)
	assert.Equal(t, user.ID, user2.ID)
	assert.Equal(t, "TESTNICK", user2.CurrentNick)
}

func TestResolveUserByKnownHostRecovery(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "Original", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	user2, err := resolveUser("testnet", "NewNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user2)
	assert.Equal(t, user.ID, user2.ID)
	assert.Equal(t, "NewNick", user2.CurrentNick)
	assert.Equal(t, "newnick", user2.NormalizedNick)
}

func TestResolveUserByKnownHostMultiMatchDisambiguation(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user1, err := createNewUser("testnet", "UserOne", "userone", "", "shared", "shared.host")
	require.NoError(t, err)

	user2, err := createNewUser("testnet", "UserTwo", "usertwo", "", "shared", "shared.host")
	require.NoError(t, err)
	assert.NotEqual(t, user1.ID, user2.ID)

	_ = upsertKnownHost(user1.ID, "shared", "shared.host")
	_ = upsertKnownHost(user2.ID, "shared", "shared.host")

	recordNickChange("testnet", "UserOne", "UserOneAlt", "rfc1459")

	recovered, err := resolveUser("testnet", "UserOneAlt", "shared", "shared.host", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, recovered)
	assert.Equal(t, user1.ID, recovered.ID, "should match user1 via nick_changes history")
}

func TestResolveUserByKnownHostAmbiguousCreatesNew(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user1, err := createNewUser("testnet", "UserOne", "userone", "", "shared", "shared.host")
	require.NoError(t, err)

	user2, err := createNewUser("testnet", "UserTwo", "usertwo", "", "shared", "shared.host")
	require.NoError(t, err)

	_ = upsertKnownHost(user1.ID, "shared", "shared.host")
	_ = upsertKnownHost(user2.ID, "shared", "shared.host")

	recovered, err := resolveUser("testnet", "UnknownNick", "shared", "shared.host", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, recovered)
	assert.NotEqual(t, user1.ID, recovered.ID)
	assert.NotEqual(t, user2.ID, recovered.ID)
}

func TestResolveUserAccountUpdatesExisting(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "Nick1", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "", user.IRCAccount)

	user2, err := resolveUser("testnet", "Nick1", "ident1", "host1", "newaccount", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user2)
	assert.Equal(t, user.ID, user2.ID)
	assert.Equal(t, "newaccount", user2.IRCAccount)
}

func TestResolveUserByNick(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	found, err := resolveUserByNick("testnet", "testnick", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, user.ID, found.ID)

	notFound, err := resolveUserByNick("testnet", "nonexistent", "rfc1459")
	require.NoError(t, err)
	assert.Nil(t, notFound)
}

func TestRecordNickChange(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "OldNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	ok := recordNickChange("testnet", "OldNick", "NewNick", "rfc1459")
	assert.True(t, ok)

	updated, err := getUserByID(user.ID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, "NewNick", updated.CurrentNick)
	assert.Equal(t, "newnick", updated.NormalizedNick)

	var changes []NickChange
	theDB.Where("user_id = ?", user.ID).Find(&changes)
	require.Len(t, changes, 1)
	assert.Equal(t, "OldNick", changes[0].OldNick)
	assert.Equal(t, "NewNick", changes[0].NewNick)
	assert.Equal(t, "oldnick", changes[0].NormalizedOld)
	assert.Equal(t, "newnick", changes[0].NormalizedNew)
}

func TestRecordNickChangeUntracked(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	ok := recordNickChange("testnet", "Nobody", "Somebody", "rfc1459")
	assert.False(t, ok)
}

func TestUpsertKnownHost(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := createNewUser("testnet", "Nick", "nick", "", "ident1", "host1")
	require.NoError(t, err)
	require.NotNil(t, user)

	var hosts []UserKnownHost
	theDB.Where("user_id = ?", user.ID).Find(&hosts)
	require.Len(t, hosts, 1)
	firstSeen := hosts[0].FirstSeen

	time.Sleep(10 * time.Millisecond)
	err = upsertKnownHost(user.ID, "ident1", "host1")
	require.NoError(t, err)

	theDB.Where("user_id = ?", user.ID).Find(&hosts)
	require.Len(t, hosts, 1)
	assert.True(t, hosts[0].LastSeen.After(firstSeen))

	err = upsertKnownHost(user.ID, "ident2", "host2")
	require.NoError(t, err)

	theDB.Where("user_id = ?", user.ID).Find(&hosts)
	require.Len(t, hosts, 2)
}

func TestResolveUserDifferentNetworks(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user1, err := resolveUser("net1", "SameNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user1)

	user2, err := resolveUser("net2", "SameNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user2)
	assert.NotEqual(t, user1.ID, user2.ID)
}

func TestResolveUserDBNil(t *testing.T) {
	oldDB := theDB
	theDB = nil
	defer func() { theDB = oldDB }()

	user, err := resolveUser("testnet", "Nick", "ident", "host", "", "rfc1459")
	assert.NoError(t, err)
	assert.Nil(t, user)
}

func TestCasemappingInResolution(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "[Test]", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "{test}", user.NormalizedNick)

	user2, err := resolveUser("testnet", "{test}", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user2)
	assert.Equal(t, user.ID, user2.ID)
}
