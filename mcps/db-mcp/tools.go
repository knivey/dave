package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type scopeFields struct {
	Network string `json:"_dave_inject_network"`
	Channel string `json:"_dave_inject_channel"`
	UserID  int64  `json:"_dave_inject_user_id"`
	Nick    string `json:"_dave_inject_nick"`
}

type ToolHandlers struct {
	mu  sync.RWMutex
	cfg Config
	db  *sqlx.DB
}

func NewToolHandlers(cfg Config, db *sqlx.DB) *ToolHandlers {
	return &ToolHandlers{cfg: cfg, db: db}
}

func (h *ToolHandlers) getConfig() Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *ToolHandlers) setConfig(cfg Config) {
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
}

type NoteItem struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	Nick      string `json:"nick"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func noteToItem(n Note) NoteItem {
	return NoteItem{
		ID:        n.ID,
		Key:       n.Key,
		Value:     n.Value,
		Nick:      n.Nick,
		CreatedAt: n.CreatedAt,
		UpdatedAt: n.UpdatedAt,
	}
}

func notesToItems(notes []Note) []NoteItem {
	items := make([]NoteItem, len(notes))
	for i, n := range notes {
		items[i] = noteToItem(n)
	}
	return items
}

type PutNoteInput struct {
	scopeFields
	Key   string `json:"key" jsonschema:"key/category for the note, required"`
	Value string `json:"value" jsonschema:"note content, required"`
}

type PutNoteOutput struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	CreatedAt string `json:"created_at"`
}

func (h *ToolHandlers) handlePutNote(ctx context.Context, req *mcp.CallToolRequest, input PutNoteInput) (*mcp.CallToolResult, PutNoteOutput, error) {
	if input.Key == "" {
		return nil, PutNoteOutput{}, fmt.Errorf("key is required")
	}
	if input.Value == "" {
		return nil, PutNoteOutput{}, fmt.Errorf("value is required")
	}

	cfg := h.getConfig()

	note, err := dbInsertNote(h.db, input.Network, input.Channel, input.UserID, input.Nick, input.Key, input.Value, cfg.Database.MaxValueSize)
	if err != nil {
		return nil, PutNoteOutput{}, fmt.Errorf("failed to insert note: %w", err)
	}

	dbPruneUserNotes(h.db, input.Network, input.UserID, cfg.Database.MaxNotesPerUser)

	return nil, PutNoteOutput{
		ID:        note.ID,
		Key:       note.Key,
		CreatedAt: note.CreatedAt,
	}, nil
}

type GetNotesInput struct {
	scopeFields
	Key        string `json:"key" jsonschema:"key to look up, required"`
	FilterNick string `json:"filter_nick,omitempty" jsonschema:"optional nick to filter by"`
}

type GetNotesOutput struct {
	Notes []NoteItem `json:"notes"`
}

func (h *ToolHandlers) handleGetNotes(ctx context.Context, req *mcp.CallToolRequest, input GetNotesInput) (*mcp.CallToolResult, GetNotesOutput, error) {
	notes, err := dbGetNotesByKey(h.db, input.Network, input.Channel, input.Key, input.FilterNick)
	if err != nil {
		return nil, GetNotesOutput{}, fmt.Errorf("failed to get notes: %w", err)
	}

	return nil, GetNotesOutput{
		Notes: notesToItems(notes),
	}, nil
}

type SearchNotesInput struct {
	scopeFields
	Query      string `json:"query" jsonschema:"search query string, required"`
	FilterKey  string `json:"filter_key,omitempty" jsonschema:"optional key filter"`
	FilterNick string `json:"filter_nick,omitempty" jsonschema:"optional nick filter"`
	Within     string `json:"within,omitempty" jsonschema:"optional time range like '1h', '7d', '30d'"`
	Limit      int    `json:"limit,omitempty" jsonschema:"optional max results, default 20"`
}

type SearchNotesOutput struct {
	Notes   []NoteItem `json:"notes"`
	Total   int        `json:"total"`
	Message string     `json:"message,omitempty"`
}

func (h *ToolHandlers) handleSearchNotes(ctx context.Context, req *mcp.CallToolRequest, input SearchNotesInput) (*mcp.CallToolResult, SearchNotesOutput, error) {
	if input.Query == "" {
		return nil, SearchNotesOutput{}, fmt.Errorf("query is required")
	}

	notes, err := dbSearchNotes(h.db, input.Network, input.Channel, input.Query, input.FilterKey, input.FilterNick, input.Within, input.Limit)
	if err != nil {
		return nil, SearchNotesOutput{}, fmt.Errorf("failed to search notes: %w", err)
	}

	output := SearchNotesOutput{
		Notes: notesToItems(notes),
		Total: len(notes),
	}

	if len(notes) == 0 {
		output.Message = "no notes found matching query"
	}

	return nil, output, nil
}

type RecentNotesInput struct {
	scopeFields
	Within     string `json:"within" jsonschema:"time range like '1h', '24h', '7d', '30d', required"`
	FilterKey  string `json:"filter_key,omitempty" jsonschema:"optional key filter"`
	FilterNick string `json:"filter_nick,omitempty" jsonschema:"optional nick filter"`
	Limit      int    `json:"limit,omitempty" jsonschema:"optional max results, default 20"`
}

type RecentNotesOutput struct {
	Notes []NoteItem `json:"notes"`
	Total int        `json:"total"`
}

func (h *ToolHandlers) handleRecentNotes(ctx context.Context, req *mcp.CallToolRequest, input RecentNotesInput) (*mcp.CallToolResult, RecentNotesOutput, error) {
	notes, err := dbRecentNotes(h.db, input.Network, input.Channel, input.Within, input.FilterKey, input.FilterNick, input.Limit)
	if err != nil {
		return nil, RecentNotesOutput{}, fmt.Errorf("failed to get recent notes: %w", err)
	}

	return nil, RecentNotesOutput{
		Notes: notesToItems(notes),
		Total: len(notes),
	}, nil
}

type DeleteNoteInput struct {
	scopeFields
	ID int64 `json:"id" jsonschema:"note ID to delete, required"`
}

type DeleteNoteOutput struct {
	Deleted bool `json:"deleted"`
}

func (h *ToolHandlers) handleDeleteNote(ctx context.Context, req *mcp.CallToolRequest, input DeleteNoteInput) (*mcp.CallToolResult, DeleteNoteOutput, error) {
	deleted, err := dbDeleteNote(h.db, input.ID, input.Network, input.Channel, input.UserID)
	if err != nil {
		return nil, DeleteNoteOutput{}, fmt.Errorf("failed to delete note: %w", err)
	}
	if !deleted {
		return nil, DeleteNoteOutput{}, fmt.Errorf("note not found or not owned by you")
	}

	return nil, DeleteNoteOutput{Deleted: true}, nil
}

type DeleteNotesInput struct {
	scopeFields
	Key string `json:"key" jsonschema:"key to delete all notes for, required"`
}

type DeleteNotesOutput struct {
	DeletedCount int64 `json:"deleted_count"`
}

func (h *ToolHandlers) handleDeleteNotes(ctx context.Context, req *mcp.CallToolRequest, input DeleteNotesInput) (*mcp.CallToolResult, DeleteNotesOutput, error) {
	affected, err := dbDeleteNotesByKey(h.db, input.Network, input.Channel, input.UserID, input.Key)
	if err != nil {
		return nil, DeleteNotesOutput{}, fmt.Errorf("failed to delete notes: %w", err)
	}

	return nil, DeleteNotesOutput{DeletedCount: affected}, nil
}

type ListKeysInput struct {
	scopeFields
	FilterNick string `json:"filter_nick,omitempty" jsonschema:"optional nick to filter by"`
	Limit      int    `json:"limit,omitempty" jsonschema:"optional max keys to return, default 50"`
}

type ListKeysOutput struct {
	Keys []KeyCount `json:"keys"`
}

func (h *ToolHandlers) handleListKeys(ctx context.Context, req *mcp.CallToolRequest, input ListKeysInput) (*mcp.CallToolResult, ListKeysOutput, error) {
	keys, err := dbListKeys(h.db, input.Network, input.Channel, input.FilterNick, input.Limit)
	if err != nil {
		return nil, ListKeysOutput{}, fmt.Errorf("failed to list keys: %w", err)
	}

	return nil, ListKeysOutput{Keys: keys}, nil
}

type CountNotesInput struct {
	scopeFields
	FilterKey  string `json:"filter_key,omitempty" jsonschema:"optional key filter"`
	FilterNick string `json:"filter_nick,omitempty" jsonschema:"optional nick filter"`
	Within     string `json:"within,omitempty" jsonschema:"optional time range like '1h', '7d', '30d'"`
}

type CountNotesOutput struct {
	Count int `json:"count"`
}

func (h *ToolHandlers) handleCountNotes(ctx context.Context, req *mcp.CallToolRequest, input CountNotesInput) (*mcp.CallToolResult, CountNotesOutput, error) {
	count, err := dbCountNotes(h.db, input.Network, input.Channel, input.FilterKey, input.FilterNick, input.Within)
	if err != nil {
		return nil, CountNotesOutput{}, fmt.Errorf("failed to count notes: %w", err)
	}

	return nil, CountNotesOutput{Count: count}, nil
}
