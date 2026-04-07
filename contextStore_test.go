package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogpt "github.com/sashabaranov/go-openai"
)

func TestMain(m *testing.M) {
	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	chatContextsMutex.Unlock()
	contextLastActive = make(map[string]int64)
	contextsModified = false

	os.Exit(m.Run())
}

func TestContextStoreRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "test_contexts.json")
	defer func() { persistCfg.FilePath = oldPath }()

	chatContextsMutex.Lock()
	chatContextsMap["testkey1"] = ChatContext{
		Messages: []gogpt.ChatCompletionMessage{
			{Role: "system", Content: "You are a helpful assistant"},
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there!"},
		},
		Config: AIConfig{MaxHistory: 5, Temperature: 0.7},
	}
	chatContextsMutex.Unlock()
	contextLastActive["testkey1"] = time.Now().Unix()

	SaveContextStore()

	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	chatContextsMutex.Unlock()
	contextLastActive = make(map[string]int64)

	LoadContextStore()

	chatContextsMutex.Lock()
	ctx, ok := chatContextsMap["testkey1"]
	chatContextsMutex.Unlock()

	if !ok {
		t.Fatal("expected context to be loaded")
	}

	if len(ctx.Messages) != 3 {
		t.Errorf("expected 3 messages, got %d", len(ctx.Messages))
	}

	if ctx.Messages[0].Role != "system" {
		t.Errorf("first message should be system, got %s", ctx.Messages[0].Role)
	}

	if ctx.Messages[0].Content != "You are a helpful assistant" {
		t.Errorf("system prompt mismatch: %s", ctx.Messages[0].Content)
	}
}

func TestContextStoreMissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "nonexistent.json")
	defer func() { persistCfg.FilePath = oldPath }()

	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	chatContextsMutex.Unlock()
	contextLastActive = make(map[string]int64)

	LoadContextStore()

	chatContextsMutex.Lock()
	if len(chatContextsMap) != 0 {
		t.Errorf("expected empty map after loading missing file, got %d", len(chatContextsMap))
	}
	chatContextsMutex.Unlock()
}

func TestContextStoreCorruptFile(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "corrupt.json")
	defer func() { persistCfg.FilePath = oldPath }()

	err := os.WriteFile(persistCfg.FilePath, []byte("not valid json{{{"), 0644)
	if err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	chatContextsMutex.Unlock()
	contextLastActive = make(map[string]int64)

	LoadContextStore()

	chatContextsMutex.Lock()
	if len(chatContextsMap) != 0 {
		t.Errorf("expected empty map after loading corrupt file, got %d", len(chatContextsMap))
	}
	chatContextsMutex.Unlock()
}

func TestContextStoreCleanupByAge(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "cleanup_test.json")
	defer func() { persistCfg.FilePath = oldPath }()

	now := time.Now().Unix()

	chatContextsMutex.Lock()
	chatContextsMap = map[string]ChatContext{
		"active": {
			Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "hi"}},
			Config:   AIConfig{MaxHistory: 5},
		},
		"old": {
			Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "old"}},
			Config:   AIConfig{MaxHistory: 5},
		},
	}
	chatContextsMutex.Unlock()

	contextLastActive = map[string]int64{
		"active": now,
		"old":    now - 10*86400,
	}

	oldMaxAge := persistCfg.MaxAgeDays
	persistCfg.MaxAgeDays = 7
	defer func() { persistCfg.MaxAgeDays = oldMaxAge }()

	CleanupContexts()

	chatContextsMutex.Lock()
	_, hasActive := chatContextsMap["active"]
	_, hasOld := chatContextsMap["old"]
	chatContextsMutex.Unlock()

	if !hasActive {
		t.Error("expected active context to remain")
	}

	if hasOld {
		t.Error("expected old context to be deleted")
	}
}

func TestContextStoreCleanupByMaxContexts(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "max_cleanup_test.json")
	defer func() { persistCfg.FilePath = oldPath }()

	now := time.Now().Unix()

	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	for i := 0; i < 15; i++ {
		chatContextsMap[string(rune('a'+i))] = ChatContext{
			Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "test"}},
			Config:   AIConfig{MaxHistory: 5},
		}
	}
	chatContextsMutex.Unlock()

	contextLastActive = make(map[string]int64)
	for i := 0; i < 15; i++ {
		contextLastActive[string(rune('a'+i))] = now - int64(i)
	}

	oldMaxContexts := persistCfg.MaxContexts
	persistCfg.MaxContexts = 10
	defer func() { persistCfg.MaxContexts = oldMaxContexts }()

	CleanupContexts()

	chatContextsMutex.Lock()
	count := len(chatContextsMap)
	chatContextsMutex.Unlock()

	if count != 10 {
		t.Errorf("expected 10 contexts after cleanup, got %d", count)
	}
}

func TestContextStoreEmptyContextsNotSaved(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "empty_test.json")
	defer func() { persistCfg.FilePath = oldPath }()

	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	chatContextsMutex.Unlock()
	contextLastActive = make(map[string]int64)

	SaveContextStore()

	data, err := os.ReadFile(persistCfg.FilePath)
	if err != nil {
		t.Fatalf("failed to read saved file: %v", err)
	}

	var store ContextStore
	err = json.Unmarshal(data, &store)
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(store.Contexts) != 0 {
		t.Errorf("expected empty contexts in saved file, got %d", len(store.Contexts))
	}
}

func TestPersistConfigSetDefaults(t *testing.T) {
	cfg := PersistConfig{}
	cfg.SetDefaults()

	if cfg.MaxAgeDays != 7 {
		t.Errorf("expected MaxAgeDays 7, got %d", cfg.MaxAgeDays)
	}

	if cfg.MaxContexts != 100 {
		t.Errorf("expected MaxContexts 100, got %d", cfg.MaxContexts)
	}
}

func TestPersistConfigSetDefaultsNoOverwrite(t *testing.T) {
	cfg := PersistConfig{
		MaxAgeDays:  30,
		MaxContexts: 50,
	}
	cfg.SetDefaults()

	if cfg.MaxAgeDays != 30 {
		t.Errorf("expected MaxAgeDays 30, got %d", cfg.MaxAgeDays)
	}

	if cfg.MaxContexts != 50 {
		t.Errorf("expected MaxContexts 50, got %d", cfg.MaxContexts)
	}
}

type mockFileStore struct {
	data         map[string][]byte
	statErr      map[string]error
	renameErr    error
	writeErr     map[string]error
	readErr      map[string]error
	statCalled   int
	writeCalled  int
	renameCalled int
}

func newMockFileStore() *mockFileStore {
	return &mockFileStore{
		data:     make(map[string][]byte),
		statErr:  make(map[string]error),
		writeErr: make(map[string]error),
		readErr:  make(map[string]error),
	}
}

func (m *mockFileStore) Read(path string) ([]byte, error) {
	if err := m.readErr[path]; err != nil {
		return nil, err
	}
	if data, ok := m.data[path]; ok {
		return data, nil
	}
	return nil, os.ErrNotExist
}

func (m *mockFileStore) Write(path string, data []byte) error {
	if err := m.writeErr[path]; err != nil {
		return err
	}
	m.writeCalled++
	m.data[path] = data
	return nil
}

func (m *mockFileStore) Rename(old, new string) error {
	if m.renameErr != nil {
		return m.renameErr
	}
	m.renameCalled++
	if data, ok := m.data[old]; ok {
		delete(m.data, old)
		m.data[new] = data
	}
	return nil
}

func (m *mockFileStore) Stat(path string) (os.FileInfo, error) {
	m.statCalled++
	if err := m.statErr[path]; err != nil {
		return nil, err
	}
	if _, ok := m.data[path]; ok {
		return nil, nil
	}
	return nil, os.ErrNotExist
}

func TestMarkContextsDirty_SetsFlag(t *testing.T) {
	oldDelay := persistCfg.SaveDelay
	persistCfg.SaveDelay = 10 * time.Millisecond
	defer func() { persistCfg.SaveDelay = oldDelay }()

	contextStoreMutex.Lock()
	contextsModified = false
	contextStoreMutex.Unlock()

	MarkContextsDirty()

	contextStoreMutex.Lock()
	if !contextsModified {
		t.Error("expected contextsModified to be true")
	}
	contextStoreMutex.Unlock()
}

func TestStartSaveTimer_SavesWhenDirty(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "save_dirty_test.json")
	defer func() { persistCfg.FilePath = oldPath }()

	mock := newMockFileStore()
	original := GetFileStore()
	SetFileStore(mock)
	defer SetFileStore(original)

	chatContextsMutex.Lock()
	chatContextsMap["test"] = ChatContext{
		Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "hi"}},
		Config:   AIConfig{MaxHistory: 5},
	}
	chatContextsMutex.Unlock()
	contextLastActive["test"] = time.Now().Unix()

	oldDelay := persistCfg.SaveDelay
	persistCfg.SaveDelay = 10 * time.Millisecond
	defer func() { persistCfg.SaveDelay = oldDelay }()

	contextStoreMutex.Lock()
	contextsModified = true
	contextStoreMutex.Unlock()

	StartSaveTimer()

	time.Sleep(50 * time.Millisecond)

	if mock.writeCalled != 1 {
		t.Errorf("expected 1 write when dirty, got %d", mock.writeCalled)
	}
}

func TestStartSaveTimer_NoSaveWhenClean(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "save_clean_test.json")
	defer func() { persistCfg.FilePath = oldPath }()

	mock := newMockFileStore()
	original := GetFileStore()
	SetFileStore(mock)
	defer SetFileStore(original)

	chatContextsMutex.Lock()
	chatContextsMap["test"] = ChatContext{
		Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "hi"}},
		Config:   AIConfig{MaxHistory: 5},
	}
	chatContextsMutex.Unlock()
	contextLastActive["test"] = time.Now().Unix()

	oldDelay := persistCfg.SaveDelay
	persistCfg.SaveDelay = 10 * time.Millisecond
	defer func() { persistCfg.SaveDelay = oldDelay }()

	contextStoreMutex.Lock()
	contextsModified = false
	contextStoreMutex.Unlock()

	StartSaveTimer()

	time.Sleep(50 * time.Millisecond)

	if mock.writeCalled != 0 {
		t.Errorf("expected 0 writes when clean, got %d", mock.writeCalled)
	}
}

func TestClearContext_RemovesTimestamp(t *testing.T) {
	key := "testkey"

	contextLastActive[key] = time.Now().Unix()

	ClearContext(key)

	if _, ok := contextLastActive[key]; ok {
		t.Error("expected timestamp to be deleted after ClearContext")
	}
}

func TestSaveContextStore_WritesTempThenRename(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "atomic_test.json")
	defer func() { persistCfg.FilePath = oldPath }()

	mock := newMockFileStore()
	original := GetFileStore()
	SetFileStore(mock)
	defer SetFileStore(original)

	chatContextsMutex.Lock()
	chatContextsMap["test"] = ChatContext{
		Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "hi"}},
		Config:   AIConfig{MaxHistory: 5},
	}
	chatContextsMutex.Unlock()
	contextLastActive["test"] = time.Now().Unix()

	SaveContextStore()

	if mock.writeCalled != 1 {
		t.Errorf("expected 1 write to temp file, got %d", mock.writeCalled)
	}

	if mock.renameCalled != 1 {
		t.Errorf("expected 1 rename, got %d", mock.renameCalled)
	}

	expectedTemp := persistCfg.FilePath + ".tmp"
	if _, ok := mock.data[expectedTemp]; ok {
		t.Error("temp file should be removed after rename")
	}
}

func TestCleanupContexts_SortsByRecentFirst(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := persistCfg.FilePath
	persistCfg.FilePath = filepath.Join(tmpDir, "sort_test.json")
	defer func() { persistCfg.FilePath = oldPath }()

	now := time.Now().Unix()

	chatContextsMutex.Lock()
	chatContextsMap = map[string]ChatContext{
		"recent1": {Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "a"}}, Config: AIConfig{MaxHistory: 5}},
		"recent2": {Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "b"}}, Config: AIConfig{MaxHistory: 5}},
		"recent3": {Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "c"}}, Config: AIConfig{MaxHistory: 5}},
		"old1":    {Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "d"}}, Config: AIConfig{MaxHistory: 5}},
		"old2":    {Messages: []gogpt.ChatCompletionMessage{{Role: "user", Content: "e"}}, Config: AIConfig{MaxHistory: 5}},
	}
	chatContextsMutex.Unlock()

	contextLastActive = map[string]int64{
		"recent1": now,
		"recent2": now - 1,
		"recent3": now - 2,
		"old1":    now - 100,
		"old2":    now - 200,
	}

	oldMaxContexts := persistCfg.MaxContexts
	persistCfg.MaxContexts = 3
	defer func() { persistCfg.MaxContexts = oldMaxContexts }()

	CleanupContexts()

	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()

	if _, ok := chatContextsMap["recent1"]; !ok {
		t.Error("recent1 (most recent) should remain")
	}
	if _, ok := chatContextsMap["recent2"]; !ok {
		t.Error("recent2 should remain")
	}
	if _, ok := chatContextsMap["recent3"]; !ok {
		t.Error("recent3 should remain")
	}
	if _, ok := chatContextsMap["old1"]; ok {
		t.Error("old1 should be deleted")
	}
	if _, ok := chatContextsMap["old2"]; ok {
		t.Error("old2 should be deleted")
	}
}

func TestPersistConfigSaveDelay(t *testing.T) {
	cfg := PersistConfig{}
	cfg.SetDefaults()

	if cfg.SaveDelay != 30*time.Second {
		t.Errorf("expected SaveDelay 30s, got %v", cfg.SaveDelay)
	}

	if cfg.FilePath != "contexts.json" {
		t.Errorf("expected FilePath 'contexts.json', got %s", cfg.FilePath)
	}
}
