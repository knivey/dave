package main

import (
	"fmt"
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

func TestReleaseUserNick(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	err = releaseUserNick(user.ID)
	require.NoError(t, err)

	updated, err := getUserByID(user.ID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, "", updated.CurrentNick)
	assert.True(t, isReleasedNick(updated.NormalizedNick))
	assert.Contains(t, updated.NormalizedNick, releasedNickPrefix)

	found, err := getUserByNormalizedNick("testnet", "testnick")
	assert.NoError(t, err)
	assert.Nil(t, found, "released nick should not be findable by original normalized_nick")
}

func TestHasNoKnownHosts(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := createNewUser("testnet", "Ghost", "ghost", "", "", "")
	require.NoError(t, err)
	require.NotNil(t, user)
	noHosts, err := hasNoKnownHosts(user.ID)
	require.NoError(t, err)
	assert.True(t, noHosts, "user with no hosts should return true")

	_ = upsertKnownHost(user.ID, "ident1", "host1")
	noHosts, err = hasNoKnownHosts(user.ID)
	require.NoError(t, err)
	assert.False(t, noHosts, "user with hosts should return false")
}

func TestMergeUser(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	ghost, err := createNewUser("testnet", "Ghost", "ghost", "", "", "")
	require.NoError(t, err)

	target, err := createNewUser("testnet", "Target", "target", "", "ident1", "host1")
	require.NoError(t, err)

	ghostSession := Session{
		Network:     "testnet",
		Channel:     "#test",
		ChatCommand: "chat",
		UserID:      &ghost.ID,
		Status:      "completed",
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	require.NoError(t, theDB.Create(&ghostSession).Error)

	ghostBan, err := createBan(theDB, ghost.ID, "testnet", "#test", "", "spam", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	require.NoError(t, theDB.Create(&NickChange{
		UserID:        ghost.ID,
		OldNick:       "OldGhost",
		NewNick:       "Ghost",
		NormalizedOld: "oldghost",
		NormalizedNew: "ghost",
		CreatedAt:     time.Now(),
	}).Error)

	err = mergeUser(ghost.ID, target.ID)
	require.NoError(t, err)

	var sessions []Session
	theDB.Where("user_id = ?", target.ID).Find(&sessions)
	assert.Len(t, sessions, 1)

	var bans []Ban
	theDB.Where("user_id = ?", target.ID).Find(&bans)
	assert.Len(t, bans, 1)
	assert.Equal(t, ghostBan.ID, bans[0].ID)

	var changes []NickChange
	theDB.Where("user_id = ?", target.ID).Find(&changes)
	assert.Len(t, changes, 1)

	deletedUser, err := getUserByID(ghost.ID)
	assert.NoError(t, err)
	assert.Nil(t, deletedUser, "ghost user should be deleted after merge")
}

func TestMergeUserReassignsBannerUserID(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	ghost, err := createNewUser("testnet", "Ghost", "ghost", "", "", "")
	require.NoError(t, err)

	target, err := createNewUser("testnet", "Target", "target", "", "", "")
	require.NoError(t, err)

	_, err = createBan(theDB, target.ID, "testnet", "#test", "", "spam", 1*time.Hour, &ghost.ID, "Ghost")
	require.NoError(t, err)

	err = mergeUser(ghost.ID, target.ID)
	require.NoError(t, err)

	var bans []Ban
	theDB.Where("user_id = ?", target.ID).Find(&bans)
	require.Len(t, bans, 1)
	assert.Equal(t, &target.ID, bans[0].BannerUserID, "banner_user_id should be reassigned to target")
}

func TestRecordNickChangeCollisionMergeGhost(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	ghost, err := createNewUser("testnet", "UserA", "usera", "", "", "")
	require.NoError(t, err)
	noHosts, err := hasNoKnownHosts(ghost.ID)
	require.NoError(t, err)
	assert.True(t, noHosts, "ghost should have no known hosts")

	userB, err := resolveUser("testnet", "UserB", "identB", "hostB", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userB)

	ok := recordNickChange("testnet", "UserB", "UserA", "rfc1459")
	assert.True(t, ok)

	deletedGhost, err := getUserByID(ghost.ID)
	assert.NoError(t, err)
	assert.Nil(t, deletedGhost, "ghost user should be merged and deleted")

	updatedB, err := getUserByID(userB.ID)
	require.NoError(t, err)
	require.NotNil(t, updatedB)
	assert.Equal(t, "UserA", updatedB.CurrentNick)
	assert.Equal(t, "usera", updatedB.NormalizedNick)

	foundByNick, err := getUserByNormalizedNick("testnet", "usera")
	require.NoError(t, err)
	require.NotNil(t, foundByNick)
	assert.Equal(t, userB.ID, foundByNick.ID, "userB should now own the nick")
}

func TestRecordNickChangeCollisionReleaseReal(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userA, err := resolveUser("testnet", "UserA", "identA", "hostA", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userA)
	noHosts, err := hasNoKnownHosts(userA.ID)
	require.NoError(t, err)
	assert.False(t, noHosts, "userA should have known hosts")

	userB, err := resolveUser("testnet", "UserB", "identB", "hostB", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userB)

	ok := recordNickChange("testnet", "UserB", "UserA", "rfc1459")
	assert.True(t, ok)

	releasedA, err := getUserByID(userA.ID)
	require.NoError(t, err)
	require.NotNil(t, releasedA)
	assert.True(t, isReleasedNick(releasedA.NormalizedNick), "userA's nick should be released")
	assert.Equal(t, "", releasedA.CurrentNick)

	updatedB, err := getUserByID(userB.ID)
	require.NoError(t, err)
	require.NotNil(t, updatedB)
	assert.Equal(t, "UserA", updatedB.CurrentNick)
	assert.Equal(t, "usera", updatedB.NormalizedNick)
}

func TestRecordNickChangeNoCollision(t *testing.T) {
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

	found, err := getUserByNormalizedNick("testnet", "oldnick")
	assert.NoError(t, err)
	assert.Nil(t, found, "old nick should no longer resolve")
}

func TestResolveUserSkipsReleasedNick(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	err = releaseUserNick(user.ID)
	require.NoError(t, err)

	resolved, err := resolveUser("testnet", "TestNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, user.ID, resolved.ID, "should recover same user via host after nick released")
}

func TestResolveUserByNickSkipsReleased(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestNick", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	err = releaseUserNick(user.ID)
	require.NoError(t, err)

	found, err := resolveUserByNick("testnet", "TestNick", "rfc1459")
	require.NoError(t, err)
	assert.Nil(t, found, "resolveUserByNick should skip released users")
}

func TestNickTakeoverScenario(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userA, err := resolveUser("testnet", "UserA", "identA", "hostA", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userA)

	sessionA := Session{
		Network:     "testnet",
		Channel:     "#test",
		ChatCommand: "chat",
		UserID:      &userA.ID,
		Status:      "completed",
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	require.NoError(t, theDB.Create(&sessionA).Error)

	userB, err := resolveUser("testnet", "UserB", "identB", "hostB", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userB)
	assert.NotEqual(t, userA.ID, userB.ID)

	sessionB := Session{
		Network:     "testnet",
		Channel:     "#test",
		ChatCommand: "chat",
		UserID:      &userB.ID,
		Status:      "completed",
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	require.NoError(t, theDB.Create(&sessionB).Error)

	releaseUserNick(userA.ID)

	ok := recordNickChange("testnet", "UserB", "UserA", "rfc1459")
	assert.True(t, ok)

	resolved, err := resolveUser("testnet", "UserA", "identB", "hostB", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, userB.ID, resolved.ID, "should resolve to userB, not userA")

	var bSessions []Session
	theDB.Where("user_id = ?", userB.ID).Find(&bSessions)
	assert.Len(t, bSessions, 1, "userB should still have their own session")
	assert.Equal(t, sessionB.ID, bSessions[0].ID)

	var aSessions []Session
	theDB.Where("user_id = ?", userA.ID).Find(&aSessions)
	assert.Len(t, aSessions, 1, "userA should still have their session (not merged, has host history)")
	assert.Equal(t, sessionA.ID, aSessions[0].ID)

	ok = recordNickChange("testnet", "UserA", "UserB", "rfc1459")
	assert.True(t, ok)

	resolvedBack, err := resolveUser("testnet", "UserB", "identB", "hostB", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, resolvedBack)
	assert.Equal(t, userB.ID, resolvedBack.ID, "userB changing back should still be userB")

	var bSessionsAfter []Session
	theDB.Where("user_id = ?", userB.ID).Find(&bSessionsAfter)
	assert.Len(t, bSessionsAfter, 1, "userB should still have their session after changing back")
}

func TestNickTakeoverMergeGhostScenario(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	ghost, err := createNewUser("testnet", "UserA", "usera", "", "", "")
	require.NoError(t, err)
	noHosts, err := hasNoKnownHosts(ghost.ID)
	require.NoError(t, err)
	assert.True(t, noHosts)

	ghostSession := Session{
		Network:     "testnet",
		Channel:     "#test",
		ChatCommand: "chat",
		UserID:      &ghost.ID,
		Status:      "completed",
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	require.NoError(t, theDB.Create(&ghostSession).Error)

	userB, err := resolveUser("testnet", "UserB", "identB", "hostB", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userB)

	sessionB := Session{
		Network:     "testnet",
		Channel:     "#test",
		ChatCommand: "chat",
		UserID:      &userB.ID,
		Status:      "completed",
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	require.NoError(t, theDB.Create(&sessionB).Error)

	ok := recordNickChange("testnet", "UserB", "UserA", "rfc1459")
	assert.True(t, ok)

	deleted, err := getUserByID(ghost.ID)
	assert.NoError(t, err)
	assert.Nil(t, deleted, "ghost should be deleted after merge")

	var allSessions []Session
	theDB.Where("user_id = ?", userB.ID).Order("id").Find(&allSessions)
	assert.Len(t, allSessions, 2, "userB should have both their session and the ghost's session")

	resolved, err := resolveUser("testnet", "UserA", "identB", "hostB", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, userB.ID, resolved.ID)
}

func TestGetUserInfo(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestUser", "ident1", "host1", "myaccount", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, user)

	session := Session{
		Network:     "testnet",
		Channel:     "#test",
		ChatCommand: "chat",
		UserID:      &user.ID,
		Status:      "completed",
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	require.NoError(t, theDB.Create(&session).Error)
	require.NoError(t, insertDBMessage(session.ID, "user", "hello", nil, nil, nil, nil))

	info, err := getUserInfo(user.ID)
	require.NoError(t, err)
	require.NotNil(t, info)

	assert.Equal(t, user.ID, info.User.ID)
	assert.Equal(t, "myaccount", info.User.IRCAccount)
	assert.Len(t, info.Hosts, 1)
	assert.Equal(t, 1, info.SessionCount)
	assert.Equal(t, 1, info.MessageCount)
	assert.Empty(t, info.ActiveBans)

	infoNil, err := getUserInfo(99999)
	assert.NoError(t, err)
	assert.Nil(t, infoNil)
}

func TestGetUserInfoWithBansAndNickChanges(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestUser", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)

	ok := recordNickChange("testnet", "TestUser", "NewNick", "rfc1459")
	assert.True(t, ok)

	_, err = createBan(theDB, user.ID, "testnet", "#test", "", "spam", 1*time.Hour, nil, "admin")
	require.NoError(t, err)

	info, err := getUserInfo(user.ID)
	require.NoError(t, err)
	require.NotNil(t, info)

	assert.Len(t, info.NickChanges, 1)
	assert.Equal(t, "TestUser", info.NickChanges[0].OldNick)
	assert.Equal(t, "NewNick", info.NickChanges[0].NewNick)
	assert.Len(t, info.ActiveBans, 1)
}

func TestSearchUsersByNick(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	_, err := resolveUser("testnet", "AlphaUser", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "BetaUser", "ident2", "host2", "betaaccount", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "GammaUser", "ident3", "host3", "", "rfc1459")
	require.NoError(t, err)

	results, err := searchUsers("testnet", "alpha")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "AlphaUser", results[0].CurrentNick)

	results, err = searchUsers("testnet", "user")
	require.NoError(t, err)
	assert.Len(t, results, 3)

	results, err = searchUsers("testnet", "betaaccount")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "BetaUser", results[0].CurrentNick)
	assert.Equal(t, "betaaccount", results[0].IRCAccount)

	results, err = searchUsers("testnet", "nonexistent")
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestSearchUsersByID(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestUser", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)

	results, err := searchUsers("testnet", fmt.Sprintf("%d", user.ID))
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, user.ID, results[0].ID)
}

func TestSearchUsersByHost(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	_, err := resolveUser("testnet", "TestUser", "myident", "myhost.example.com", "", "rfc1459")
	require.NoError(t, err)

	results, err := searchUsers("testnet", "myhost")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "TestUser", results[0].CurrentNick)
	assert.Equal(t, 1, results[0].HostCount)
}

func TestSearchUsersReleased(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestUser", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)

	require.NoError(t, releaseUserNick(user.ID))

	results, err := searchUsers("testnet", fmt.Sprintf("%d", user.ID))
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Released)
}

func TestSearchUsersDifferentNetwork(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	_, err := resolveUser("testnet", "TestUser", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)

	results, err := searchUsers("othernet", "testuser")
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestSearchUsersWildcardAll(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	_, err := resolveUser("testnet", "AlphaUser", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "BetaUser", "ident2", "host2", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "GammaUser", "ident3", "host3", "", "rfc1459")
	require.NoError(t, err)

	results, err := searchUsers("testnet", "*")
	require.NoError(t, err)
	assert.Len(t, results, 3)
}

func TestSearchUsersWildcardPrefix(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	_, err := resolveUser("testnet", "FooBar", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "BarFoo", "ident2", "host2", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "BazFooQux", "ident3", "host3", "", "rfc1459")
	require.NoError(t, err)

	results, err := searchUsers("testnet", "Foo*")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "FooBar", results[0].CurrentNick)
}

func TestSearchUsersWildcardSuffix(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	_, err := resolveUser("testnet", "FooBar", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "BarFoo", "ident2", "host2", "", "rfc1459")
	require.NoError(t, err)

	results, err := searchUsers("testnet", "*Bar")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "FooBar", results[0].CurrentNick)
}

func TestSearchUsersWildcardContains(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	_, err := resolveUser("testnet", "FooBar", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "XFooBarX", "ident2", "host2", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "BarFoo", "ident3", "host3", "", "rfc1459")
	require.NoError(t, err)

	results, err := searchUsers("testnet", "*Foo*")
	require.NoError(t, err)
	assert.Len(t, results, 3)

	results, err = searchUsers("testnet", "Foo")
	require.NoError(t, err)
	assert.Len(t, results, 3, "plain query without * should also be contains match")
}

func TestSearchUsersWildcardMiddle(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	_, err := resolveUser("testnet", "FooBarBaz", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "FooBaz", "ident2", "host2", "", "rfc1459")
	require.NoError(t, err)
	_, err = resolveUser("testnet", "BazFoo", "ident3", "host3", "", "rfc1459")
	require.NoError(t, err)

	results, err := searchUsers("testnet", "Foo*Baz")
	require.NoError(t, err)
	assert.Len(t, results, 2, "Foo*Baz should match FooBarBaz and FooBaz (SQL % matches zero chars)")
	nicks := make(map[string]bool)
	for _, r := range results {
		nicks[r.CurrentNick] = true
	}
	assert.True(t, nicks["FooBarBaz"])
	assert.True(t, nicks["FooBaz"])
	assert.False(t, nicks["BazFoo"])
}

func TestComputeMergeHash(t *testing.T) {
	ghost := &User{ID: 1, CurrentNick: "Ghost"}
	target := &User{ID: 2, CurrentNick: "Target"}

	hash1 := computeMergeHash(ghost, target)
	hash2 := computeMergeHash(ghost, target)
	assert.Equal(t, hash1, hash2, "same inputs should produce same hash")
	assert.Len(t, hash1, 8, "hash should be 8 characters")

	ghostModified := &User{ID: 1, CurrentNick: "GhostModified"}
	hash3 := computeMergeHash(ghostModified, target)
	assert.NotEqual(t, hash1, hash3, "different nicks should produce different hash")

	swapped := computeMergeHash(target, ghost)
	assert.NotEqual(t, hash1, swapped, "swapped order should produce different hash")
}

func TestGetUserDBStatsAllNetworks(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	user, err := resolveUser("testnet", "TestUser", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)

	s1 := Session{
		Network: "testnet", Channel: "#chan1", ChatCommand: "chat",
		UserID: &user.ID, Status: "completed",
		CreatedAt: time.Now(), LastActive: time.Now(),
	}
	require.NoError(t, theDB.Create(&s1).Error)
	require.NoError(t, insertDBMessage(s1.ID, "user", "hello", nil, nil, nil, nil))
	require.NoError(t, insertDBMessage(s1.ID, "assistant", "hi", nil, nil, nil, nil))

	s2 := Session{
		Network: "testnet", Channel: "#chan2", ChatCommand: "chat",
		UserID: &user.ID, Status: "completed",
		CreatedAt: time.Now(), LastActive: time.Now(),
	}
	require.NoError(t, theDB.Create(&s2).Error)
	require.NoError(t, insertDBMessage(s2.ID, "user", "msg", nil, nil, nil, nil))

	sessions, messages, err := getUserDBStatsAllNetworks(user.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, sessions)
	assert.Equal(t, 3, messages)

	sessions, messages, err = getUserDBStatsAllNetworks(99999)
	require.NoError(t, err)
	assert.Equal(t, 0, sessions)
	assert.Equal(t, 0, messages)
}

// TestResolveUserAccountPathReleasesCollidingRealUser reproduces the
// production incident where account-matched user #18 (released) tried to
// reclaim normalized_nick "spartan" while another user (#172) still owned it,
// causing a UNIQUE constraint failure on (network, normalized_nick).
// claimNickFor must displace the colliding real user before the update.
func TestResolveUserAccountPathReleasesCollidingRealUser(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	// userA: the real "SpartaN" with account, currently released.
	userA, err := resolveUser("gamesurge", "SpartaN", "identA", "hostA", "SpartaN[DK]", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userA)
	require.NoError(t, releaseUserNick(userA.ID))

	// userB: someone else grabbed the nick "SpartaN" while A was released.
	userB, err := resolveUser("gamesurge", "SpartaN", "identB", "hostB", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userB)
	require.NotEqual(t, userA.ID, userB.ID)
	require.Equal(t, "spartan", userB.NormalizedNick)

	// A returns: account match + nick collision with B must resolve cleanly.
	resolved, err := resolveUser("gamesurge", "SpartaN", "identA", "hostA", "SpartaN[DK]", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, userA.ID, resolved.ID)
	assert.Equal(t, "SpartaN", resolved.CurrentNick)
	assert.Equal(t, "spartan", resolved.NormalizedNick)
	assert.Equal(t, "SpartaN[DK]", resolved.IRCAccount)

	// B should still exist but be released.
	displacedB, err := getUserByID(userB.ID)
	require.NoError(t, err)
	require.NotNil(t, displacedB)
	assert.True(t, isReleasedNick(displacedB.NormalizedNick), "userB's nick should now be released")

	// Lookup by normalized nick should resolve to A.
	foundByNick, err := getUserByNormalizedNick("gamesurge", "spartan")
	require.NoError(t, err)
	require.NotNil(t, foundByNick)
	assert.Equal(t, userA.ID, foundByNick.ID)
}

// TestResolveUserAccountPathMergesGhostHoldingNick covers the ghost-merge
// branch: when the colliding user has no known hosts, they are merged into
// the account-matched user (deleted, with all their sessions reassigned).
func TestResolveUserAccountPathMergesGhostHoldingNick(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	// userA: real user with account.
	userA, err := resolveUser("testnet", "RealNick", "identA", "hostA", "myacct", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userA)
	require.NoError(t, releaseUserNick(userA.ID))

	// userB: migration-era ghost holding "newname" with no known hosts.
	ghost, err := createNewUser("testnet", "NewName", "newname", "", "", "")
	require.NoError(t, err)
	require.NotNil(t, ghost)
	noHosts, err := hasNoKnownHosts(ghost.ID)
	require.NoError(t, err)
	require.True(t, noHosts, "ghost must have no known hosts for merge path")

	// Give the ghost a couple of sessions to verify reassignment.
	s1 := Session{
		Network: "testnet", Channel: "#chan1", ChatCommand: "chat",
		UserID: &ghost.ID, Status: "completed",
		CreatedAt: time.Now(), LastActive: time.Now(),
	}
	require.NoError(t, theDB.Create(&s1).Error)
	s2 := Session{
		Network: "testnet", Channel: "#chan2", ChatCommand: "chat",
		UserID: &ghost.ID, Status: "completed",
		CreatedAt: time.Now(), LastActive: time.Now(),
	}
	require.NoError(t, theDB.Create(&s2).Error)

	// A returns under the ghost's nick — should merge ghost into A.
	resolved, err := resolveUser("testnet", "NewName", "identA", "hostA", "myacct", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, userA.ID, resolved.ID)
	assert.Equal(t, "newname", resolved.NormalizedNick)
	assert.Equal(t, "NewName", resolved.CurrentNick)

	// Ghost row must be deleted.
	deletedGhost, err := getUserByID(ghost.ID)
	assert.NoError(t, err)
	assert.Nil(t, deletedGhost, "ghost user should have been merged and deleted")

	// Ghost's sessions must now belong to A.
	var s1After, s2After Session
	require.NoError(t, theDB.First(&s1After, s1.ID).Error)
	require.NoError(t, theDB.First(&s2After, s2.ID).Error)
	require.NotNil(t, s1After.UserID)
	require.NotNil(t, s2After.UserID)
	assert.Equal(t, userA.ID, *s1After.UserID)
	assert.Equal(t, userA.ID, *s2After.UserID)
}

// TestResolveUserAccountPathSameNickNoOp confirms the common case (no
// collision, account-matched user already has the right normalized_nick)
// triggers no extra DB work and produces no error.
func TestResolveUserAccountPathSameNickNoOp(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	userA, err := resolveUser("testnet", "Stable", "ident1", "host1", "acct", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, userA)
	require.Equal(t, "stable", userA.NormalizedNick)

	resolved, err := resolveUser("testnet", "Stable", "ident1", "host1", "acct", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, userA.ID, resolved.ID)
	assert.Equal(t, "stable", resolved.NormalizedNick)
	assert.Equal(t, "Stable", resolved.CurrentNick)

	// Only one user row exists for this network.
	var count int64
	require.NoError(t, theDB.Model(&User{}).Where("network = ?", "testnet").Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

// TestClaimNickForHelper exercises the helper directly across all four
// branches: same-nick no-op, free-nick no-op, ghost collision (merge), and
// real-user collision (release).
func TestClaimNickForHelper(t *testing.T) {
	t.Run("same nick is no-op", func(t *testing.T) {
		cleanup := setupUserTestDB(t)
		defer cleanup()

		user, err := resolveUser("net", "Alice", "ident", "host", "", "rfc1459")
		require.NoError(t, err)
		err = claimNickFor("net", user, "alice")
		require.NoError(t, err)

		// User unchanged.
		reloaded, err := getUserByID(user.ID)
		require.NoError(t, err)
		assert.Equal(t, "alice", reloaded.NormalizedNick)
	})

	t.Run("free nick is no-op", func(t *testing.T) {
		cleanup := setupUserTestDB(t)
		defer cleanup()

		user, err := resolveUser("net", "Alice", "ident", "host", "", "rfc1459")
		require.NoError(t, err)
		err = claimNickFor("net", user, "freenick")
		require.NoError(t, err)

		// No row should hold "freenick" yet.
		found, err := getUserByNormalizedNick("net", "freenick")
		require.NoError(t, err)
		assert.Nil(t, found)
	})

	t.Run("ghost collision triggers merge", func(t *testing.T) {
		cleanup := setupUserTestDB(t)
		defer cleanup()

		user, err := resolveUser("net", "Alice", "ident", "host", "", "rfc1459")
		require.NoError(t, err)

		ghost, err := createNewUser("net", "Bob", "bob", "", "", "")
		require.NoError(t, err)

		err = claimNickFor("net", user, "bob")
		require.NoError(t, err)

		deleted, err := getUserByID(ghost.ID)
		assert.NoError(t, err)
		assert.Nil(t, deleted, "ghost should have been merged and deleted")
	})

	t.Run("real-user collision triggers release", func(t *testing.T) {
		cleanup := setupUserTestDB(t)
		defer cleanup()

		userA, err := resolveUser("net", "Alice", "identA", "hostA", "", "rfc1459")
		require.NoError(t, err)
		userB, err := resolveUser("net", "Bob", "identB", "hostB", "", "rfc1459")
		require.NoError(t, err)

		err = claimNickFor("net", userA, "bob")
		require.NoError(t, err)

		releasedB, err := getUserByID(userB.ID)
		require.NoError(t, err)
		require.NotNil(t, releasedB)
		assert.True(t, isReleasedNick(releasedB.NormalizedNick), "userB should be released")
	})
}

func TestIsTransientDBErr(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"database is locked", true},
		{"database is busy", true},
		{"SQLITE_BUSY: database is busy", true},
		{"deadlock detected", true},
		{"could not serialize access due to concurrent update", true},
		{"UNIQUE constraint failed: users.normalized_nick", false},
		{"some random error", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = fmt.Errorf("%s", c.msg)
		}
		assert.Equalf(t, c.want, isTransientDBErr(err), "msg=%q", c.msg)
	}
	assert.False(t, isTransientDBErr(nil))
}

func TestIsUniqueConstraintErr(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"UNIQUE constraint failed: users.network, users.normalized_nick", true},
		{"ERROR: duplicate key value violates unique constraint \"idx_users_nick\"", true},
		{"SQLSTATE 23505", true},
		{"pq: duplicate key value violates unique constraint (SQLSTATE 23505)", true},
		{"database is locked", false},
		{"some random error", false},
		// Bare numeric substring must NOT trigger — predicate requires the
		// full "SQLSTATE 23505" prefix to avoid false positives where
		// "23505" appears coincidentally (e.g. a row ID, a port number).
		{"error: row 235050 not found", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = fmt.Errorf("%s", c.msg)
		}
		assert.Equalf(t, c.want, isUniqueConstraintErr(err), "msg=%q", c.msg)
	}
	assert.False(t, isUniqueConstraintErr(nil))
}

func TestIsPlaceholderNick(t *testing.T) {
	assert.True(t, isReleasedNick(",quit,18"))
	assert.True(t, isFlaggedNick(",flagged,gamesurge,spartan,1700000000000"))
	assert.True(t, isPlaceholderNick(",quit,18"))
	assert.True(t, isPlaceholderNick(",flagged,net,foo,1"))
	assert.False(t, isPlaceholderNick("spartan"))
	assert.False(t, isPlaceholderNick(""))
}

// TestResolveUserFallbackCreatesFlaggedRow simulates the case where
// claimNickFor cannot resolve the collision (e.g. a race we didn't anticipate)
// by overriding claimNickForFn to a no-op. The subsequent UPDATE then trips
// the UNIQUE constraint, and resolveUser must fall back to a flagged row.
func TestResolveUserFallbackCreatesFlaggedRow(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	// userA: real user, account, with host. Will be released to set up the
	// "I have an account but the nick is taken" scenario.
	userA, err := resolveUser("gamesurge", "SpartaN", "identA", "hostA", "SpartaN[DK]", "rfc1459")
	require.NoError(t, err)
	require.NoError(t, releaseUserNick(userA.ID))

	// userB grabs the nick.
	userB, err := resolveUser("gamesurge", "SpartaN", "identB", "hostB", "", "rfc1459")
	require.NoError(t, err)
	require.NotEqual(t, userA.ID, userB.ID)

	// Force claimNickFor to a no-op so the impending UPDATE will fail.
	saved := claimNickForFn
	claimNickForFn = func(network string, user *User, norm string) error { return nil }
	defer func() { claimNickForFn = saved }()

	// Now SpartaN tries to reclaim; claimNickFor (no-op) does nothing,
	// updateDBUser hits UNIQUE constraint, resolveUser falls back.
	resolved, err := resolveUser("gamesurge", "SpartaN", "identA", "hostA", "SpartaN[DK]", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.True(t, resolved.Flagged, "fallback row must be flagged")
	assert.True(t, isFlaggedNick(resolved.NormalizedNick), "normalized_nick should be flagged sentinel, got %q", resolved.NormalizedNick)
	assert.Equal(t, FlaggedReasonCollision, resolved.FlaggedReason)
	assert.Equal(t, "SpartaN[DK]", resolved.IRCAccount)
	assert.Equal(t, "SpartaN", resolved.CurrentNick)
	// Distinct from both prior rows.
	assert.NotEqual(t, userA.ID, resolved.ID)
	assert.NotEqual(t, userB.ID, resolved.ID)
}

func TestFlaggedUserRowSkippedByAccountLookup(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	// Real user with account.
	realUser, err := resolveUser("net", "RealNick", "identR", "hostR", "shared_account", "rfc1459")
	require.NoError(t, err)

	// Manually create a flagged row that also carries the same account.
	flagged := &User{
		Network:        "net",
		CurrentNick:    "Imposter",
		NormalizedNick: ",flagged,net,imposter,1700000000000",
		IRCAccount:     "shared_account",
		Flagged:        true,
		FlaggedReason:  FlaggedReasonCollision,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	require.NoError(t, theDB.Create(flagged).Error)

	found, err := getUserByAccount("net", "shared_account")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, realUser.ID, found.ID, "getUserByAccount must skip flagged rows")
}

func TestFlaggedUserRowSkippedByNickLookup(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	// Only a flagged row exists with the target normalized_nick sentinel.
	flagged := &User{
		Network:        "net",
		CurrentNick:    "Ghost",
		NormalizedNick: ",flagged,net,ghost,1700000000000",
		Flagged:        true,
		FlaggedReason:  FlaggedReasonCollision,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	require.NoError(t, theDB.Create(flagged).Error)

	// Looking up by the "real" normalized nick must not return the flagged row.
	found, err := getUserByNormalizedNick("net", "ghost")
	require.NoError(t, err)
	assert.Nil(t, found, "ghost nick is only held by flagged sentinel, lookup should miss")
}

func TestGetFlaggedUsers(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	// 2 flagged users on 'net', 1 flagged on 'other', 1 normal.
	for i, spec := range []struct {
		net     string
		nick    string
		norm    string
		flagged bool
	}{
		{"net", "A", "a", true},
		{"net", "B", "b", true},
		{"other", "C", "c", true},
		{"net", "D", "d", false},
	} {
		var nn string
		if spec.flagged {
			nn = fmt.Sprintf(",flagged,%s,%s,%d", spec.net, spec.norm, 1700000000000+int64(i))
		} else {
			nn = spec.norm
		}
		u := &User{
			Network:        spec.net,
			CurrentNick:    spec.nick,
			NormalizedNick: nn,
			Flagged:        spec.flagged,
			FlaggedReason:  "",
			CreatedAt:      time.Now().Add(time.Duration(i) * time.Second),
			UpdatedAt:      time.Now(),
		}
		if spec.flagged {
			u.FlaggedReason = FlaggedReasonCollision
		}
		require.NoError(t, theDB.Create(u).Error)
	}

	all, err := getFlaggedUsers("")
	require.NoError(t, err)
	assert.Len(t, all, 3, "all flagged across networks")

	netOnly, err := getFlaggedUsers("net")
	require.NoError(t, err)
	assert.Len(t, netOnly, 2, "filter by network")
	for _, u := range netOnly {
		assert.Equal(t, "net", u.Network)
		assert.True(t, u.Flagged)
	}
}

func TestCountFlaggedUsers(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	n, err := countFlaggedUsers()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	flagged := &User{
		Network:        "net",
		CurrentNick:    "X",
		NormalizedNick: ",flagged,net,x,1700000000000",
		Flagged:        true,
		FlaggedReason:  FlaggedReasonCollision,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	require.NoError(t, theDB.Create(flagged).Error)

	// Add a non-flagged row to ensure the filter works.
	_, err = resolveUser("net", "Normal", "ident", "host", "", "rfc1459")
	require.NoError(t, err)

	n, err = countFlaggedUsers()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestFlaggedSentinelUnique(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	cause := fmt.Errorf("UNIQUE constraint failed: synthetic")
	u1, err := resolveUserFallback("net", "Same", "ident", "host", "", "rfc1459", cause)
	require.NoError(t, err)
	require.NotNil(t, u1)

	// No sleep: sentinel combines UnixNano with a process-local atomic
	// counter, so back-to-back calls must produce distinct sentinels
	// regardless of clock resolution.
	u2, err := resolveUserFallback("net", "Same", "ident", "host", "", "rfc1459", cause)
	require.NoError(t, err)
	require.NotNil(t, u2)

	assert.NotEqual(t, u1.ID, u2.ID)
	assert.NotEqual(t, u1.NormalizedNick, u2.NormalizedNick,
		"two fallback rows for the same nick must have distinct sentinels")
	assert.True(t, isFlaggedNick(u1.NormalizedNick))
	assert.True(t, isFlaggedNick(u2.NormalizedNick))
}

// TestResolveUserHostRecoverySkipsFlagged asserts that recoverByKnownHost
// does not return a flagged row even when it owns the queried (ident, host).
// Without the flagged=false filter on the JOIN, the flagged row created by
// resolveUserFallback (which inherits the legitimate owner's host via
// upsertKnownHost) would be re-surfaced on the next message and become a
// vector for displacing real users via claimNickFor.
func TestResolveUserHostRecoverySkipsFlagged(t *testing.T) {
	cleanup := setupUserTestDB(t)
	defer cleanup()

	cause := fmt.Errorf("UNIQUE constraint failed: synthetic")
	flagged, err := resolveUserFallback("net", "Original", "ident1", "host1", "acct1", "rfc1459", cause)
	require.NoError(t, err)
	require.NotNil(t, flagged)
	require.True(t, flagged.Flagged)

	// Sanity: flagged row owns (ident1, host1).
	var hosts []UserKnownHost
	require.NoError(t, theDB.Where("user_id = ?", flagged.ID).Find(&hosts).Error)
	require.Len(t, hosts, 1)
	assert.Equal(t, "ident1", hosts[0].Ident)
	assert.Equal(t, "host1", hosts[0].Host)

	// New resolveUser call with same ident/host but a different nick and no
	// account. The nick path misses (no row holds "different"), no account
	// branch runs, and host recovery must NOT return the flagged row. Result:
	// a fresh real user is created.
	resolved, err := resolveUser("net", "Different", "ident1", "host1", "", "rfc1459")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.NotEqual(t, flagged.ID, resolved.ID, "host recovery must not surface a flagged row")
	assert.False(t, resolved.Flagged, "newly created user should not be flagged")
	assert.Equal(t, "different", resolved.NormalizedNick)
}
