package main

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	gogpt "github.com/sashabaranov/go-openai"
)

var loggerCS = logxi.New("contextStore")

type FileStore interface {
	Read(path string) ([]byte, error)
	Write(path string, data []byte) error
	Rename(old, new string) error
	Stat(path string) (os.FileInfo, error)
}

type defaultFileStore struct{}

func (fs *defaultFileStore) Read(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (fs *defaultFileStore) Write(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}

func (fs *defaultFileStore) Rename(old, new string) error {
	return os.Rename(old, new)
}

func (fs *defaultFileStore) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

var fileStore FileStore = &defaultFileStore{}

func SetFileStore(fs FileStore) {
	fileStore = fs
}

func GetFileStore() FileStore {
	return fileStore
}

type PersistedContext struct {
	Messages   []gogpt.ChatCompletionMessage `json:"messages"`
	LastActive int64                         `json:"last_active"`
	Config     AIConfig                      `json:"config"`
}

type ContextStore struct {
	Contexts map[string]PersistedContext `json:"contexts"`
}

type PersistConfig struct {
	MaxAgeDays  int           `toml:"max_age_days"`
	MaxContexts int           `toml:"max_contexts"`
	SaveDelay   time.Duration `toml:"save_delay"`
	FilePath    string        `toml:"file_path"`
}

var persistCfg PersistConfig

var contextStoreMutex sync.Mutex
var contextStoreSaveTimer *time.Timer
var contextsModified bool
var contextLastActive map[string]int64

func init() {
	loggerCS.SetLevel(logxi.LevelAll)
	persistCfg = DefaultPersistConfig()
	persistCfg.SetDefaults()
	contextLastActive = make(map[string]int64)
}

func SaveContextStore() {
	store := ContextStore{
		Contexts: make(map[string]PersistedContext),
	}

	chatContextsMutex.Lock()
	for key, ctx := range chatContextsMap {
		if len(ctx.Messages) > 0 {
			store.Contexts[key] = PersistedContext{
				Messages:   ctx.Messages,
				LastActive: contextLastActive[key],
				Config:     ctx.Config,
			}
		}
	}
	chatContextsMutex.Unlock()

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		loggerCS.Error("Failed to marshal context store", "error", err)
		return
	}

	tmpFile := persistCfg.FilePath + ".tmp"
	err = fileStore.Write(tmpFile, data)
	if err != nil {
		loggerCS.Error("Failed to write temp file", "error", err)
		return
	}

	err = fileStore.Rename(tmpFile, persistCfg.FilePath)
	if err != nil {
		loggerCS.Error("Failed to rename temp file", "error", err)
		return
	}
	loggerCS.Debug("Saved context store", "contexts", len(store.Contexts))
}

func LoadContextStore() {
	_, err := fileStore.Stat(persistCfg.FilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		loggerCS.Error("Failed to stat context store", "error", err)
		return
	}

	data, err := fileStore.Read(persistCfg.FilePath)
	if err != nil {
		loggerCS.Error("Failed to read context store", "error", err)
		return
	}

	var store ContextStore
	err = json.Unmarshal(data, &store)
	if err != nil {
		loggerCS.Error("Failed to parse context store", "error", err)
		return
	}

	chatContextsMutex.Lock()
	for key, pctx := range store.Contexts {
		chatContextsMap[key] = ChatContext{
			Messages: pctx.Messages,
			Config:   pctx.Config,
		}
		contextLastActive[key] = pctx.LastActive
	}
	chatContextsMutex.Unlock()

	loggerCS.Debug("Loaded context store", "contexts", len(store.Contexts))
}

func CleanupContexts() {
	if persistCfg.MaxAgeDays == 0 && persistCfg.MaxContexts == 0 {
		return
	}

	contextStoreMutex.Lock()
	defer contextStoreMutex.Unlock()

	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()

	now := time.Now().Unix()
	cutoff := now - int64(persistCfg.MaxAgeDays*86400)

	var toDelete []string

	for key := range chatContextsMap {
		if persistCfg.MaxAgeDays > 0 {
			lastActive, ok := contextLastActive[key]
			if !ok || lastActive < cutoff {
				toDelete = append(toDelete, key)
			}
		}
	}

	for _, key := range toDelete {
		delete(chatContextsMap, key)
		delete(contextLastActive, key)
	}

	if persistCfg.MaxContexts > 0 && len(chatContextsMap) > persistCfg.MaxContexts {
		type keyTime struct {
			key      string
			lastUsed int64
		}

		var byTime []keyTime
		for key := range chatContextsMap {
			lastUsed := contextLastActive[key]
			byTime = append(byTime, keyTime{key: key, lastUsed: lastUsed})
		}

		sort.Slice(byTime, func(i, j int) bool {
			return byTime[i].lastUsed < byTime[j].lastUsed
		})

		count := len(chatContextsMap) - persistCfg.MaxContexts
		for i := 0; i < count; i++ {
			delete(chatContextsMap, byTime[i].key)
			delete(contextLastActive, byTime[i].key)
		}
	}

	if len(toDelete) > 0 {
		loggerCS.Info("Cleaned up old contexts", "count", len(toDelete))
	}
}

func MarkContextsDirty() {
	contextStoreMutex.Lock()
	defer contextStoreMutex.Unlock()
	contextsModified = true
}

func StartSaveTimer() {
	contextStoreMutex.Lock()
	defer contextStoreMutex.Unlock()

	delay := persistCfg.SaveDelay
	if delay == 0 {
		delay = 30 * time.Second
	}

	contextStoreSaveTimer = time.AfterFunc(delay, func() {
		contextStoreMutex.Lock()
		if contextsModified {
			contextsModified = false
			contextStoreMutex.Unlock()
			SaveContextStore()
		} else {
			contextStoreMutex.Unlock()
		}
		StartSaveTimer()
	})
}

func StopPendingSave() {
	contextStoreMutex.Lock()
	defer contextStoreMutex.Unlock()
	contextsModified = false
	if contextStoreSaveTimer != nil {
		contextStoreSaveTimer.Stop()
	}
}

func (p *PersistConfig) SetDefaults() {
	if p.MaxAgeDays == 0 {
		p.MaxAgeDays = 7
	}
	if p.MaxContexts == 0 {
		p.MaxContexts = 100
	}
	if p.SaveDelay == 0 {
		p.SaveDelay = 30 * time.Second
	}
	if p.FilePath == "" {
		p.FilePath = "contexts.json"
	}
}

func DefaultPersistConfig() PersistConfig {
	return PersistConfig{
		MaxAgeDays:  7,
		MaxContexts: 100,
		SaveDelay:   30 * time.Second,
		FilePath:    "contexts.json",
	}
}
