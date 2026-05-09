package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Session struct {
	ID           int64          `gorm:"primaryKey;autoIncrement"`
	Network      string         `gorm:"not null"`
	Channel      string         `gorm:"not null"`
	Nick         string         `gorm:"not null"`
	ChatCommand  string         `gorm:"column:chat_command;not null"`
	FirstMessage string         `gorm:"column:first_message;not null;default:''"`
	ConvID       *string        `gorm:"column:conv_id"`
	ResponseID   *string        `gorm:"column:response_id"`
	Service      string         `gorm:"not null;default:''"`
	Model        string         `gorm:"not null;default:''"`
	Status       string         `gorm:"not null;default:'active'"`
	CreatedAt    time.Time
	LastActive   time.Time      `gorm:"column:last_active"`
	DeletedAt    gorm.DeletedAt `gorm:"index"`
	SettingsID   *int64         `gorm:"index:idx_sessions_settings"`
}

type SessionSetting struct {
	ID               int64  `gorm:"primaryKey;autoIncrement"`
	System           string `gorm:"type:text"`
	Model            string
	DetectImages     bool
	MaxImages        int
	MaxContextImages int
	ReasoningEffort  string
	CreatedAt        time.Time
}

type Message struct {
	ID               int64   `gorm:"primaryKey;autoIncrement"`
	SessionID        int64   `gorm:"not null"`
	Role             string  `gorm:"not null"`
	Content          string  `gorm:"not null;type:text"`
	ToolCalls        *string `gorm:"type:text"`
	ToolCallID       *string
	ReasoningContent *string `gorm:"type:text"`
	MultiContent     *string `gorm:"type:text"`
	IsAsyncResult    bool    `gorm:"default:false"`
	SettingsID       *int64  `gorm:"index:idx_messages_settings"`
	CreatedAt        time.Time
}

type PendingJob struct {
	ID          int64   `gorm:"primaryKey;autoIncrement"`
	SessionID   *int64  `gorm:""`
	JobID       string  `gorm:"not null"`
	ToolName    string  `gorm:"not null"`
	MCPServer   string  `gorm:"not null"`
	Status      string  `gorm:"not null;default:'pending'"`
	Result      *string `gorm:"type:text"`
	Network     *string
	Channel     *string
	Nick        *string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

type TurnUsage struct {
	ID               int64  `gorm:"primaryKey;autoIncrement"`
	SessionID        int64  `gorm:"not null"`
	PromptTokens     int    `gorm:"not null;default:0"`
	CompletionTokens int    `gorm:"not null;default:0"`
	CachedTokens     int    `gorm:"not null;default:0"`
	ReasoningTokens  int    `gorm:"not null;default:0"`
	FinishReason     string `gorm:"not null;default:''"`
	APIPath          string `gorm:"not null;default:''"`
	DurationMs       int    `gorm:"not null;default:0"`
	CreatedAt        time.Time
}

func (TurnUsage) TableName() string { return "turn_usage" }

func (SessionSetting) TableName() string { return "session_settings" }

func migrateTable[T any](src, dst *gorm.DB, batchSize int, label string) error {
	var count int64
	src.Model(new(T)).Count(&count)
	fmt.Printf("Found %d %s\n", count, label)
	if count == 0 {
		return nil
	}

	var all []T
	if err := src.Order("id ASC").FindInBatches(&all, batchSize, func(_ *gorm.DB, _ int) error {
		return nil
	}).Error; err != nil {
		return fmt.Errorf("reading %s: %w", label, err)
	}

	if err := dst.CreateInBatches(&all, batchSize).Error; err != nil {
		return fmt.Errorf("writing %s: %w", label, err)
	}
	fmt.Printf("Migrated %d %s\n", len(all), label)
	return nil
}

func main() {
	sqlitePath := flag.String("sqlite", "", "Path to SQLite database file (required)")
	pgDSN := flag.String("postgres", "", "PostgreSQL DSN, e.g. postgres://user:pass@localhost:5432/dave?sslmode=disable (required)")
	dryRun := flag.Bool("dry-run", false, "Show what would be migrated without writing")
	flag.Parse()

	if *sqlitePath == "" || *pgDSN == "" {
		fmt.Fprintln(os.Stderr, "Usage: migrate-sqlite-to-pgsql --sqlite <path> --postgres <dsn> [--dry-run]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	srcDB, err := gorm.Open(sqlite.Open(*sqlitePath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open SQLite: %v\n", err)
		os.Exit(1)
	}
	sqlSrc, _ := srcDB.DB()
	defer sqlSrc.Close()

	dstDB, err := gorm.Open(postgres.Open(*pgDSN), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to PostgreSQL: %v\n", err)
		os.Exit(1)
	}
	sqlDst, _ := dstDB.DB()
	defer sqlDst.Close()

	if *dryRun {
		var count int64
		srcDB.Model(&Session{}).Count(&count)
		fmt.Printf("Would migrate %d sessions\n", count)
		srcDB.Model(&Message{}).Count(&count)
		fmt.Printf("Would migrate %d messages\n", count)
		srcDB.Model(&PendingJob{}).Count(&count)
		fmt.Printf("Would migrate %d pending_jobs\n", count)
		srcDB.Model(&TurnUsage{}).Count(&count)
		fmt.Printf("Would migrate %d turn_usage records\n", count)
		fmt.Println("Dry run complete — no data was written.")
		return
	}

	fmt.Println("Running AutoMigrate on PostgreSQL...")
	if err := dstDB.AutoMigrate(&Session{}, &SessionSetting{}, &Message{}, &PendingJob{}, &TurnUsage{}); err != nil {
		fmt.Fprintf(os.Stderr, "AutoMigrate failed: %v\n", err)
		os.Exit(1)
	}

	dstDB.Exec("SET session_replication_role = 'replica'")

	if err := migrateTable[Session](srcDB, dstDB, 100, "sessions"); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}
	if err := migrateTable[SessionSetting](srcDB, dstDB, 100, "session_settings"); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}
	if err := migrateTable[Message](srcDB, dstDB, 500, "messages"); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}
	if err := migrateTable[PendingJob](srcDB, dstDB, 100, "pending_jobs"); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}
	if err := migrateTable[TurnUsage](srcDB, dstDB, 500, "turn_usage records"); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}

	dstDB.Exec("SET session_replication_role = 'DEFAULT'")

	tables := []string{"sessions", "session_settings", "messages", "pending_jobs", "turn_usage"}
	for _, table := range tables {
		dstDB.Exec(fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', 'id'), COALESCE((SELECT MAX(id) FROM %s), 0))", table, table))
	}
	fmt.Println("Reset PostgreSQL sequences")
	fmt.Println("Migration complete!")
}
