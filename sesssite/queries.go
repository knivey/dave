package main

import (
	"time"

	"gorm.io/gorm"
)

func sessionBySlug(db *gorm.DB, slug string) (*Session, error) {
	var s Session
	if err := db.Where("slug = ?", slug).First(&s).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func userBySlug(db *gorm.DB, slug string) (*User, error) {
	var u User
	if err := db.Where("slug = ?", slug).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// liveTranscriptCond is the transcript-liveness predicate shared by
// EVERY query that fetches or counts transcript rows: tail-copies
// (source_compaction_id set — compaction re-inserted rows whose
// content duplicates the archived originals) and superseded rows are
// excluded, so the flat transcript shows each message exactly once
// (spec). It lived as four drifting string literals before this
// constant; appending a new liveness rule here fixes every consumer.
const liveTranscriptCond = "superseded = false AND source_compaction_id IS NULL"

// sessionMessagesPage returns up to limit messages ending at beforeID
// (exclusive; 0 = newest), oldest-first. Tail-copies and superseded
// rows are excluded via liveTranscriptCond (see its comment).
func sessionMessagesPage(db *gorm.DB, sessionID int64, beforeID int64, limit int) ([]Message, error) {
	q := db.Where("session_id = ? AND "+liveTranscriptCond, sessionID)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	var msgs []Message
	if err := q.Order("id DESC").Limit(limit).Find(&msgs).Error; err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

func sessionMessageByID(db *gorm.DB, sessionID, messageID int64) (*Message, error) {
	var m Message
	if err := db.Where("session_id = ? AND id = ?", sessionID, messageID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// userSessionsPage keyset-paginates by (last_active DESC, id DESC) and
// returns the page NEWEST-FIRST (unlike sessionMessagesPage, whose
// transcript is chronological — the plan's Task 8 user page renders rows
// top-down and carries the LAST row — the oldest of the batch — as the
// "load older" cursor). The tuple compare (last_active, id) < (?, ?) is
// a row-value comparison — sqlite supports row values since 3.15
// (modernc's pure-Go engine is well past that) and postgres natively.
// id is the tiebreak for sessions sharing a last_active timestamp.
func userSessionsPage(db *gorm.DB, userID int64, beforeLastActive time.Time, beforeID int64, limit int) ([]Session, error) {
	q := db.Where("user_id = ?", userID)
	if !beforeLastActive.IsZero() {
		q = q.Where("(last_active, id) < (?, ?)", beforeLastActive, beforeID)
	}
	var sessions []Session
	if err := q.Order("last_active DESC, id DESC").Limit(limit).Find(&sessions).Error; err != nil {
		return nil, err
	}
	return sessions, nil
}

func sessionUsageRows(db *gorm.DB, sessionID int64) ([]TurnUsage, error) {
	var rows []TurnUsage
	err := db.Where("session_id = ?", sessionID).Order("created_at ASC, id ASC").Find(&rows).Error
	return rows, err
}

func sessionCompactions(db *gorm.DB, sessionID int64) ([]Compaction, error) {
	var rows []Compaction
	err := db.Where("session_id = ?", sessionID).Order("id ASC").Find(&rows).Error
	return rows, err
}
