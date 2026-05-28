# Session Clone Lineage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `cloned_from_id` and `cloned_from_nick` fields to sessions so users can see which session a clone came from.

**Architecture:** Two new nullable/default-empty columns on `sessions`. Write path sets them in `cloneDBSession`. Read path resolves the source nick via a batch lookup with `ClonedFromNick` as fallback. Displayed inline in session list lines and via `{cloned_from}` placeholder in detail header.

**Tech Stack:** Go, GORM, SQLite/PostgreSQL migrations, TOML notices config.

---

### Task 1: Schema — Add Session struct fields and migration #8

**Files:**
- Modify: `db.go:46-62` (Session struct)
- Modify: `migrations.go:36-44` (migrations slice + new function)

- [ ] **Step 1: Add fields to Session struct**

In `db.go`, add two fields after `UserID` at line 61:

```go
type Session struct {
	ID            int64   `gorm:"primaryKey;autoIncrement"`
	Network       string  `gorm:"not null;index:idx_sessions_user"`
	Channel       string  `gorm:"not null;index:idx_sessions_user"`
	ChatCommand   string  `gorm:"column:chat_command;not null"`
	FirstMessage  string  `gorm:"column:first_message;not null;default:''"`
	ConvID        *string `gorm:"column:conv_id;index:idx_sessions_conv_id"`
	ResponseID    *string `gorm:"column:response_id;index:idx_sessions_response_id"`
	Service       string  `gorm:"not null;default:''"`
	Model         string  `gorm:"not null;default:''"`
	Status        string  `gorm:"not null;default:'active';index:idx_sessions_status"`
	CreatedAt     time.Time
	LastActive    time.Time      `gorm:"column:last_active;index:idx_sessions_last_active"`
	DeletedAt     gorm.DeletedAt `gorm:"index"`
	SettingsID    *int64         `gorm:"index:idx_sessions_settings"`
	UserID        *int64         `gorm:"index:idx_sessions_user"`
	ClonedFromID  *int64         `gorm:"column:cloned_from_id"`
	ClonedFromNick string        `gorm:"column:cloned_from_nick;not null;default:''"`
}
```

- [ ] **Step 2: Add migration #8 function and register it**

In `migrations.go`, add the migration function at the end of the file (before the closing of the file, but after `convertSentinelsToReleasedColumn`):

```go
func addSessionsClonedFrom(db *gorm.DB) error {
	migrator := db.Migrator()

	if !migrator.HasColumn(&Session{}, "cloned_from_id") {
		var ddl string
		switch db.Dialector.Name() {
		case "sqlite":
			ddl = "ALTER TABLE sessions ADD COLUMN cloned_from_id INTEGER REFERENCES sessions(id)"
		case "postgres":
			ddl = "ALTER TABLE sessions ADD COLUMN cloned_from_id INTEGER REFERENCES sessions(id)"
		default:
			if err := migrator.AddColumn(&Session{}, "ClonedFromID"); err != nil {
				return fmt.Errorf("adding cloned_from_id column: %w", err)
			}
			ddl = ""
		}
		if ddl != "" {
			if err := db.Exec(ddl).Error; err != nil {
				return fmt.Errorf("adding cloned_from_id column: %w", err)
			}
		}
	}

	if !migrator.HasColumn(&Session{}, "cloned_from_nick") {
		var ddl string
		switch db.Dialector.Name() {
		case "sqlite":
			ddl = "ALTER TABLE sessions ADD COLUMN cloned_from_nick TEXT NOT NULL DEFAULT ''"
		case "postgres":
			ddl = "ALTER TABLE sessions ADD COLUMN cloned_from_nick TEXT NOT NULL DEFAULT ''"
		default:
			if err := migrator.AddColumn(&Session{}, "ClonedFromNick"); err != nil {
				return fmt.Errorf("adding cloned_from_nick column: %w", err)
			}
			ddl = ""
		}
		if ddl != "" {
			if err := db.Exec(ddl).Error; err != nil {
				return fmt.Errorf("adding cloned_from_nick column: %w", err)
			}
		}
	}

	return nil
}
```

Also add it to the `migrations` slice:

```go
var migrations = []migration{
	{ID: 1, Name: "drop_sessions_context_key", Up: dropSessionsContextKey},
	{ID: 2, Name: "create_users_from_sessions", Up: createUsersFromSessions},
	{ID: 3, Name: "normalize_channels_and_reindex", Up: normalizeChannelsAndReindex},
	{ID: 4, Name: "drop_sessions_nick", Up: dropSessionsNick},
	{ID: 5, Name: "add_users_flagged_columns", Up: addUsersFlaggedColumns},
	{ID: 6, Name: "add_users_last_nick", Up: addUsersLastNick},
	{ID: 7, Name: "convert_sentinels_to_released_column", Up: convertSentinelsToReleasedColumn},
	{ID: 8, Name: "add_sessions_cloned_from", Up: addSessionsClonedFrom},
}
```

- [ ] **Step 3: Run `go fmt` and `go vet`**

Run: `go fmt ./... && go vet ./...`
Expected: no errors

- [ ] **Step 4: Commit**

```bash
git add db.go migrations.go
git commit -m "feat: add cloned_from_id and cloned_from_nick columns to sessions"
```

---

### Task 2: Write path — Update cloneDBSession to set lineage fields

**Files:**
- Modify: `db.go:668` (cloneDBSession function)
- Modify: `historyCmds.go:726` (historyClone caller)

- [ ] **Step 1: Update cloneDBSession signature and implementation**

Change the function signature from:

```go
func cloneDBSession(sourceSessionID int64, targetNetwork, targetChannel string, targetUserID int64, systemPrompt string) (int64, error) {
```

to:

```go
func cloneDBSession(sourceSessionID int64, targetNetwork, targetChannel string, targetUserID int64, systemPrompt string, sourceNick string) (int64, error) {
```

Inside the function, after loading the source session (line 680-683), resolve the source owner's current nick. Update the new `Session` struct creation (lines 707-718) to include the new fields. The new session creation becomes:

```go
	newSession := Session{
		Network:        targetNetwork,
		Channel:        targetChannel,
		UserID:         &targetUserID,
		ChatCommand:    source.ChatCommand,
		ConvID:         &convID,
		ResponseID:     nil,
		Service:        source.Service,
		Model:          source.Model,
		Status:         StatusActive,
		SettingsID:     newSettingsID,
		ClonedFromID:   &source.ID,
		ClonedFromNick: sourceNick,
	}
```

The `sourceNick` parameter comes from the caller (which already resolves it). If the caller passes `""`, `ClonedFromNick` is empty string and the display fallback handles it.

- [ ] **Step 2: Update all callers of cloneDBSession**

All callers pass the source nick as a new last argument.

In `historyCmds.go`, the `historyClone` function (around line 726), the caller already resolves `sourceNick`. Change:

```go
	newSessionID, cloneErr := cloneDBSession(sourceSession.ID, network.Name, channel, callingUserID, systemContent)
```

to:

```go
	var resolvedSourceNick string
	if sourceNick != "" {
		resolvedSourceNick = sourceNick
	} else if sourceSession.UserID != nil {
		var srcUser User
		if err := theDB.Where("id = ?", *sourceSession.UserID).First(&srcUser).Error; err == nil && srcUser.ID != 0 {
			resolvedSourceNick = displayNick(&srcUser)
		}
	}

	newSessionID, cloneErr := cloneDBSession(sourceSession.ID, network.Name, channel, callingUserID, systemContent, resolvedSourceNick)
```

This reuses the same nick resolution that was previously done only for the notice template (lines 739-748). That existing notice-resolution code can be simplified since `resolvedSourceNick` is now available:

```go
	vars := map[string]string{
		"id":           fmt.Sprintf("%d", newSessionID),
		"source_id":    fmt.Sprintf("%d", sourceSession.ID),
		"count":        "0",
		"source_nick":  resolvedSourceNick,
	}
```

Remove the old `if sourceNick != ""` / `else` block (lines 739-748) since the resolution now happens before the call.

- [ ] **Step 3: Update all test callers of cloneDBSession**

In `clone_test.go`, every call to `cloneDBSession` needs the new argument. All existing tests clone without caring about lineage, so pass `""`:

Search for `cloneDBSession(` in `clone_test.go` and add `, ""` before the closing `)` on each call. There are calls at lines: 139, 180, 205, 230, 260, 300, 339, 700.

- [ ] **Step 4: Run `go vet`**

Run: `go vet ./...`
Expected: no errors (compiles with new signature, all callers updated)

- [ ] **Step 5: Commit**

```bash
git add db.go historyCmds.go clone_test.go
git commit -m "feat: set cloned_from_id and cloned_from_nick in cloneDBSession"
```

---

### Task 3: Write failing test for cloneDBSession lineage fields

**Files:**
- Modify: `clone_test.go`

- [ ] **Step 1: Write the test**

Add this test after `TestCloneDBSession_BasicClone` (around line 164):

```go
func TestCloneDBSession_LineageFieldsSet(t *testing.T) {
	setupTestDB(t)

	srcUserID := ensureTestUser(t, "net", "srcnick")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "chat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hi"}))

	tgtUserID := ensureTestUser(t, "net", "tgt")
	newSid, err := cloneDBSession(srcSid, "net", "#c", tgtUserID, "new sys", "srcnick")
	require.NoError(t, err)

	newSession, err := sessionMgr.GetSession(newSid)
	require.NoError(t, err)

	require.NotNil(t, newSession.ClonedFromID, "cloned session should have cloned_from_id set")
	assert.Equal(t, srcSid, *newSession.ClonedFromID, "cloned_from_id should point to source session")
	assert.Equal(t, "srcnick", newSession.ClonedFromNick, "cloned_from_nick should match source nick")
}
```

Also add a test for the empty-nick case:

```go
func TestCloneDBSession_LineageFieldsEmptyNick(t *testing.T) {
	setupTestDB(t)

	srcUserID := ensureTestUser(t, "net", "srcnick")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "chat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))

	tgtUserID := ensureTestUser(t, "net", "tgt")
	newSid, err := cloneDBSession(srcSid, "net", "#c", tgtUserID, "new sys", "")
	require.NoError(t, err)

	newSession, err := sessionMgr.GetSession(newSid)
	require.NoError(t, err)

	require.NotNil(t, newSession.ClonedFromID)
	assert.Equal(t, "", newSession.ClonedFromNick, "cloned_from_nick should be empty when sourceNick is empty")
}
```

- [ ] **Step 2: Run tests to verify they pass**

Run: `go test -run 'TestCloneDBSession_Lineage' -v`
Expected: PASS (the implementation from Task 2 already sets the fields)

- [ ] **Step 3: Commit**

```bash
git add clone_test.go
git commit -m "test: add lineage field tests for cloneDBSession"
```

---

### Task 4: Read path — Add resolveClonedFromNicks helper and formatClonedFrom

**Files:**
- Modify: `historyCmds.go` (add helper functions)

- [ ] **Step 1: Add resolveClonedFromNicks batch helper**

Add this function in `historyCmds.go` (near the top, after the imports/package):

```go
func resolveClonedFromNicks(sessions []Session) map[int64]string {
	var ids []int64
	for _, s := range sessions {
		if s.ClonedFromID != nil {
			ids = append(ids, *s.ClonedFromID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	type sourceInfo struct {
		ID    int64
		Nick  string
	}
	var results []sourceInfo
	theDB.Model(&Session{}).
		Select("sessions.id, users.current_nick as nick").
		Joins("JOIN users ON users.id = sessions.user_id").
		Where("sessions.id IN ?", ids).
		Find(&results)

	m := make(map[int64]string, len(results))
	for _, r := range results {
		m[r.ID] = r.Nick
	}
	return m
}

func formatClonedFrom(s Session, sourceNicks map[int64]string) string {
	if s.ClonedFromID == nil {
		return ""
	}
	id := *s.ClonedFromID
	if nick, ok := sourceNicks[id]; ok {
		return fmt.Sprintf(" \x0314[cloned from #%d by %s]\x0F", id, nick)
	}
	if s.ClonedFromNick != "" {
		return fmt.Sprintf(" \x0314[cloned from #%d by %s]\x0F", id, s.ClonedFromNick)
	}
	return fmt.Sprintf(" \x0314[cloned from #%d (deleted session)]\x0F", id)
}
```

- [ ] **Step 2: Run `go vet`**

Run: `go vet ./...`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add historyCmds.go
git commit -m "feat: add resolveClonedFromNicks and formatClonedFrom helpers"
```

---

### Task 5: Display — Show cloned-from in IRC session list lines

**Files:**
- Modify: `historyCmds.go:162-257` (sendSessionsLines and sendSessionsLinesWithNick)

- [ ] **Step 1: Update sendSessionsLines to append cloned-from suffix**

The `sendSessionsLines` function renders session lines. Before the rendering loop, batch-resolve source nicks. After each line's preview, append the cloned-from suffix.

Add before the first `for i, s := range sessions` loop (line 179):

```go
	sourceNicks := resolveClonedFromNicks(extractSessionsFromSWU(sessions))
```

We need a helper to extract `[]Session` from `[]SessionWithUser`. Add this small helper:

```go
func extractSessionsFromSWU(swu []SessionWithUser) []Session {
	sessions := make([]Session, len(swu))
	for i, s := range swu {
		sessions[i] = s.Session
	}
	return sessions
}
```

In the rendering loop, after `preview` is set (line 202-205), add:

```go
		var clonedSuffix string
		if s.ClonedFromID != nil {
			clonedSuffix = formatClonedFrom(s.Session, sourceNicks)
		}
```

Update the `sessionLine` struct to include `clonedFrom string`:

```go
	type sessionLine struct {
		icon      string
		nickStr   string
		idStr     string
		msgStr    string
		timeStr   string
		cmd       string
		preview   string
		clonedFrom string
	}
```

Update the line assignment (line 207):

```go
		lines[i] = sessionLine{icon, nickStr, idStr, msgStr, timeStr, s.ChatCommand, preview, clonedSuffix}
```

In the rendering section (after line 247 where preview is appended), add:

```go
		if l.clonedFrom != "" {
			line += l.clonedFrom
		}
```

This goes after the `if l.preview != "" { line += " " + l.preview }` block (around line 247-249), inside the same `for _, l := range lines` loop.

- [ ] **Step 2: Update sendSessionsLinesWithNick similarly**

`sendSessionsLinesWithNick` (line 158) just delegates:

```go
func sendSessionsLinesWithNick(output chan<- string, ctx context.Context, sessions []SessionWithUser, trigger string) {
	sendSessionsLines(output, ctx, sessions, trigger, true)
}
```

Since `sendSessionsLines` now handles the cloned-from suffix internally (via the `sourceNicks` batch call), `sendSessionsLinesWithNick` needs no additional changes.

- [ ] **Step 3: Run `go vet`**

Run: `go vet ./...`
Expected: no errors

- [ ] **Step 4: Commit**

```bash
git add historyCmds.go
git commit -m "feat: show cloned-from info in IRC session list lines"
```

---

### Task 6: Display — Show cloned-from in session detail header

**Files:**
- Modify: `notices.go:206-207` (default template)
- Modify: `historyCmds.go:303-313` (historyShow detail header expansion)
- Modify: `config/notices.toml:24,137` (documentation and commented example)

- [ ] **Step 1: Update the default detail_header template in notices.go**

Change the default (line 207) from:

```go
		n.Sessions.DetailHeader = "\x02Session #{id} ({command}) — {count} messages{archived_suffix}:\x02"
```

to:

```go
		n.Sessions.DetailHeader = "\x02Session #{id} ({command}) — {count} messages{archived_suffix}:\x02{cloned_from}"
```

- [ ] **Step 2: Update historyShow to populate {cloned_from}**

In `historyShow` (around line 303), add the `cloned_from` placeholder to the `expandNotice` vars map. After the existing vars, before the `sendOrDone` call:

```go
	var clonedFromStr string
	if session.ClonedFromID != nil {
		sourceNicks := resolveClonedFromNicks([]Session{session})
		clonedFromStr = formatClonedFrom(session, sourceNicks)
	}
```

Then add to the map:

```go
		"cloned_from": clonedFromStr,
```

- [ ] **Step 3: Update config/notices.toml documentation**

Update line 24 to add `{cloned_from}` to the placeholder list:

```toml
#   detail_header    (string)    Session detail header. Placeholders: {id}, {command}, {count}, {active}, {archived}, {archived_suffix}, {total}, {cloned_from}
```

Update the commented example at line 137:

```toml
# detail_header = "\x02Session #{id} ({command}) — {count} messages{archived_suffix}:\x02{cloned_from}"
```

- [ ] **Step 4: Run `go vet`**

Run: `go vet ./...`
Expected: no errors

- [ ] **Step 5: Commit**

```bash
git add notices.go historyCmds.go config/notices.toml
git commit -m "feat: show cloned-from in session detail header via {cloned_from} placeholder"
```

---

### Task 7: Display — Show cloned-from in TUI session list

**Files:**
- Modify: `tui_commands.go:535-654` (tuiCmdSessions)

- [ ] **Step 1: Add cloned-from suffix to TUI session lines**

The TUI `tuiCmdSessions` function has its own rendering loop (lines 600-635). Add batch resolution before the loop, and append the suffix after each line.

Add a TUI-specific format helper (uses tview color tags instead of IRC codes):

```go
func formatClonedFromTUI(s Session, sourceNicks map[int64]string) string {
	if s.ClonedFromID == nil {
		return ""
	}
	id := *s.ClonedFromID
	if nick, ok := sourceNicks[id]; ok {
		return fmt.Sprintf(" [yellow][cloned from #%d by %s][white]", id, tview.Escape(nick))
	}
	if s.ClonedFromNick != "" {
		return fmt.Sprintf(" [yellow][cloned from #%d by %s][white]", id, tview.Escape(s.ClonedFromNick))
	}
	return fmt.Sprintf(" [yellow][cloned from #%d (deleted session)][white]", id)
}
```

Before the first `for i, s := range sessions` loop (line 600), add:

```go
	sourceNicks := resolveClonedFromNicks(sessions)
```

Update the `sessionLine` struct (line 586-594) to include `clonedFrom`:

```go
	type sessionLine struct {
		icon       string
		idStr      string
		channel    string
		msgStr     string
		timeStr    string
		cmd        string
		preview    string
		clonedFrom string
	}
```

In the loop, after `preview` is set (lines 616-619), add:

```go
		var clonedSuffix string
		if s.ClonedFromID != nil {
			clonedSuffix = formatClonedFromTUI(s, sourceNicks)
		}
```

Update the line assignment (line 620):

```go
		lines[i] = sessionLine{icon, idStr, tview.Escape(s.Channel), msgStr, timeStr, tview.Escape(s.ChatCommand), tview.Escape(preview), clonedSuffix}
```

After each line is rendered in the second loop (after the preview append), add:

```go
		if l.clonedFrom != "" {
			line += l.clonedFrom
		}
```

- [ ] **Step 2: Run `go vet`**

Run: `go vet ./...`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add tui_commands.go
git commit -m "feat: show cloned-from info in TUI session list"
```

---

### Task 8: Tests — formatClonedFrom and resolveClonedFromNicks

**Files:**
- Modify: `clone_test.go`

- [ ] **Step 1: Write tests for formatClonedFrom**

```go
func TestFormatClonedFrom_NotCloned(t *testing.T) {
	s := Session{}
	assert.Equal(t, "", formatClonedFrom(s, nil))
}

func TestFormatClonedFrom_SourceExists(t *testing.T) {
	id := int64(42)
	s := Session{ClonedFromID: &id}
	sourceNicks := map[int64]string{42: "alice"}
	result := formatClonedFrom(s, sourceNicks)
	assert.Contains(t, result, "#42")
	assert.Contains(t, result, "alice")
	assert.Contains(t, result, "cloned from")
}

func TestFormatClonedFrom_SourceDeletedNickSnapshot(t *testing.T) {
	id := int64(42)
	s := Session{ClonedFromID: &id, ClonedFromNick: "bob"}
	sourceNicks := map[int64]string{}
	result := formatClonedFrom(s, sourceNicks)
	assert.Contains(t, result, "#42")
	assert.Contains(t, result, "bob")
}

func TestFormatClonedFrom_SourceDeletedNoSnapshot(t *testing.T) {
	id := int64(42)
	s := Session{ClonedFromID: &id, ClonedFromNick: ""}
	sourceNicks := map[int64]string{}
	result := formatClonedFrom(s, sourceNicks)
	assert.Contains(t, result, "#42")
	assert.Contains(t, result, "deleted session")
}
```

- [ ] **Step 2: Write test for resolveClonedFromNicks**

```go
func TestResolveClonedFromNicks(t *testing.T) {
	setupTestDB(t)

	srcUserID := ensureTestUser(t, "net", "alice")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "chat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))

	tgtUserID := ensureTestUser(t, "net", "tgt")
	newSid, err := cloneDBSession(srcSid, "net", "#c", tgtUserID, "sys", "alice")
	require.NoError(t, err)

	newSession, err := sessionMgr.GetSession(newSid)
	require.NoError(t, err)

	sessions := []Session{*newSession}
	nicks := resolveClonedFromNicks(sessions)
	assert.Equal(t, "alice", nicks[srcSid])
}

func TestResolveClonedFromNicks_NoClonedSessions(t *testing.T) {
	setupTestDB(t)

	uid := ensureTestUser(t, "net", "u1")
	sid, err := sessionMgr.CreateSession("net", "#c", uid, "chat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))

	sessions := []Session{{}}
	nicks := resolveClonedFromNicks(sessions)
	assert.Nil(t, nicks)
}
```

- [ ] **Step 3: Run tests**

Run: `go test -run 'TestFormatClonedFrom|TestResolveClonedFromNicks' -v`
Expected: all PASS

- [ ] **Step 4: Run full test suite**

Run: `go test ./...`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add clone_test.go
git commit -m "test: add tests for formatClonedFrom and resolveClonedFromNicks"
```

---

### Task 9: Final verification

- [ ] **Step 1: Run full test suite**

Run: `go test ./...`
Expected: all PASS

- [ ] **Step 2: Run go fmt and go vet**

Run: `go fmt ./... && go vet ./...`
Expected: no output (clean)

- [ ] **Step 3: Build the binary**

Run: `go build -o dave .`
Expected: success
