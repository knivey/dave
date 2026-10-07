package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// Config is the full imgsite configuration. The path fields in Database and
// Storage hold the values exactly as written in the TOML file; main resolves
// them against the binary directory into the Resolved* fields (mirroring
// img-mcp), and reloads preserve the resolved copies untouched.
type Config struct {
	Server     ServerConfig     `toml:"server"`
	Database   DatabaseConfig   `toml:"database"`
	Storage    StorageConfig    `toml:"storage"`
	Auth       AuthConfig       `toml:"auth"`
	Thumbnails ThumbnailsConfig `toml:"thumbnails"`
	Upload     UploadConfig     `toml:"upload"`
	Search     SearchConfig     `toml:"search"`
	Site       SiteConfig       `toml:"site"`
	// Reactions carries the emoji-reaction preset. An absent section
	// runs the built-in default set (normalize fills it); Enabled is
	// a pointer so "reactions.enabled = false" (explicit disable)
	// stays distinguishable from an absent section (default true).
	Reactions ReactionConfig `toml:"reactions"`
	// SafeSite is nil unless a [safe_site] section is present; nil is
	// single-site behavior identical to today. Pointer (not value) so
	// absence stays distinguishable from an empty-but-present section,
	// which validation rejects.
	SafeSite *SafeSiteConfig `toml:"safe_site"`
}

type ServerConfig struct {
	Name string `toml:"name"`
	Addr string `toml:"addr"`
	// BaseURL is the external URL used to build absolute upload-response
	// links (url/page). Empty = derive from the request: scheme from TLS
	// or X-Forwarded-Proto, host from the Host header.
	BaseURL string `toml:"base_url"`
}

type DatabaseConfig struct {
	Path     string `toml:"path"`
	Resolved string `toml:"-"`
}

type StorageConfig struct {
	Path       string `toml:"path"`
	ThumbsPath string `toml:"thumbs_path"`

	ResolvedPath       string `toml:"-"`
	ResolvedThumbsPath string `toml:"-"`
}

type AuthConfig struct {
	APIKey string `toml:"api_key"`
}

type ThumbnailsConfig struct {
	SmallWidth   int `toml:"small_width"`
	DisplayWidth int `toml:"display_width"`
	JPEGQuality  int `toml:"jpeg_quality"`
	// Workers is the only non-reloadable thumbnails field — the worker
	// pool is sized once at startup.
	Workers      int `toml:"workers"`
	MaxDimension int `toml:"max_dimension"`
}

type UploadConfig struct {
	MaxBytes      int64 `toml:"max_bytes"`
	RatePerMinute int   `toml:"rate_per_minute"`
}

type SearchConfig struct {
	SnippetChars int `toml:"snippet_chars"`
	PrefixMin    int `toml:"prefix_min"`
}

type SiteConfig struct {
	Title       string `toml:"title"`
	Description string `toml:"description"`
}

// SafeSiteConfig is the optional second logical site served from the
// same process, selected by the request Host header (design:
// docs/superpowers/specs/2026-09-26-safe-site-design.md). The safe
// site shows only images whose provenance network is in
// AllowedNetworks (case-insensitive) or whose safety verdict is
// 'safe' — 'unknown' is default-deny. All three fields are required
// when the section is present; see loadConfig.
type SafeSiteConfig struct {
	Hosts           []string `toml:"hosts"`
	BaseURL         string   `toml:"base_url"`
	AllowedNetworks []string `toml:"allowed_networks"`
}

// ReactionConfig is the emoji-reaction preset (reactions.go). Enabled
// is a POINTER: nil (section absent, or the key unwritten) means true,
// so the lightest config runs the built-in default set; an explicit
// `enabled = false` turns the whole feature off (no button row,
// endpoints 404, no card badges) without touching stored rows.
// Emojis is ordered — the array-of-tables order is the button-row
// order and the card-badge tie-break order.
type ReactionConfig struct {
	Enabled *bool           `toml:"enabled"`
	Emojis  []ReactionEmoji `toml:"emoji"`
}

// ReactionEmoji is one preset entry: Name is the stable ASCII key
// stored in the reactions table (and carried in the toggle URL);
// Glyph is what every render surface displays. Split so curating the
// glyph never needs a data migration — rows key on the name only.
type ReactionEmoji struct {
	Name  string `toml:"name"`
	Glyph string `toml:"glyph"`
}

const (
	defaultServerName     = "imgsite"
	defaultServerAddr     = ":8081"
	defaultDatabasePath   = "data/imgsite.db"
	defaultStoragePath    = "data/images"
	defaultThumbsPath     = "data/thumbs"
	defaultSmallWidth     = 480
	defaultDisplayWidth   = 1280
	defaultJPEGQuality    = 78
	defaultThumbWorkers   = 2
	defaultMaxDimension   = 8192
	defaultUploadMaxBytes = 32 << 20
	defaultRatePerMinute  = 30
	defaultSnippetChars   = 160
	defaultPrefixMin      = 2
	defaultSiteTitle      = "dave's image dump"
	defaultSiteDesc       = "generations from IRC"
	// maxReactionEmojis caps the preset so the details-page button
	// row and every card's badge space stay bounded no matter how
	// enthusiastic the curation gets.
	maxReactionEmojis = 12
)

// placeholderAPIKeyPrefix is the prefix of the placeholder secret shipped
// in config.toml's live section and the commented example block; loadConfig
// refuses it so an unedited config cannot reach production.
const placeholderAPIKeyPrefix = "CHANGE_ME"

func loadConfig(configFile string) (Config, error) {
	var cfg Config

	if _, err := toml.DecodeFile(configFile, &cfg); err != nil {
		return cfg, fmt.Errorf("loading %s: %w", configFile, err)
	}

	if cfg.Auth.APIKey == "" {
		return cfg, fmt.Errorf("auth.api_key is required")
	}
	// Reject the documented placeholder so a copy-pasted example config
	// fails fast at startup instead of running with a guessable key.
	// example.toml keeps the placeholder as documentation; only loaded
	// configs are validated.
	if strings.HasPrefix(cfg.Auth.APIKey, placeholderAPIKeyPrefix) {
		return cfg, fmt.Errorf("auth.api_key is still set to the example placeholder (%s…): set a real secret", placeholderAPIKeyPrefix)
	}

	cfg.Server.Name = defaultString(cfg.Server.Name, defaultServerName)
	cfg.Server.Addr = defaultString(cfg.Server.Addr, defaultServerAddr)
	// BaseURL may stay empty: upload responses then derive the base from
	// the incoming request.

	cfg.Database.Path = defaultString(cfg.Database.Path, defaultDatabasePath)
	cfg.Storage.Path = defaultString(cfg.Storage.Path, defaultStoragePath)
	cfg.Storage.ThumbsPath = defaultString(cfg.Storage.ThumbsPath, defaultThumbsPath)

	if cfg.Thumbnails.SmallWidth == 0 {
		cfg.Thumbnails.SmallWidth = defaultSmallWidth
	}
	if cfg.Thumbnails.DisplayWidth == 0 {
		cfg.Thumbnails.DisplayWidth = defaultDisplayWidth
	}
	if cfg.Thumbnails.JPEGQuality == 0 {
		cfg.Thumbnails.JPEGQuality = defaultJPEGQuality
	}
	if cfg.Thumbnails.Workers == 0 {
		cfg.Thumbnails.Workers = defaultThumbWorkers
	}
	if cfg.Thumbnails.MaxDimension == 0 {
		cfg.Thumbnails.MaxDimension = defaultMaxDimension
	}
	if cfg.Thumbnails.SmallWidth < 1 || cfg.Thumbnails.DisplayWidth < 1 {
		return cfg, fmt.Errorf("thumbnails.small_width and thumbnails.display_width must be positive")
	}
	if cfg.Thumbnails.JPEGQuality < 1 || cfg.Thumbnails.JPEGQuality > 100 {
		return cfg, fmt.Errorf("thumbnails.jpeg_quality must be between 1 and 100")
	}
	if cfg.Thumbnails.Workers < 1 {
		return cfg, fmt.Errorf("thumbnails.workers must be at least 1")
	}
	if cfg.Thumbnails.MaxDimension < 1 {
		return cfg, fmt.Errorf("thumbnails.max_dimension must be positive")
	}

	if cfg.Upload.MaxBytes == 0 {
		cfg.Upload.MaxBytes = defaultUploadMaxBytes
	}
	if cfg.Upload.RatePerMinute == 0 {
		cfg.Upload.RatePerMinute = defaultRatePerMinute
	}
	if cfg.Upload.MaxBytes < 1 {
		return cfg, fmt.Errorf("upload.max_bytes must be positive")
	}
	if cfg.Upload.RatePerMinute < 1 {
		return cfg, fmt.Errorf("upload.rate_per_minute must be positive")
	}

	if cfg.Search.SnippetChars == 0 {
		cfg.Search.SnippetChars = defaultSnippetChars
	}
	if cfg.Search.PrefixMin == 0 {
		cfg.Search.PrefixMin = defaultPrefixMin
	}
	if cfg.Search.SnippetChars < 1 {
		return cfg, fmt.Errorf("search.snippet_chars must be positive")
	}
	if cfg.Search.PrefixMin < 1 {
		return cfg, fmt.Errorf("search.prefix_min must be at least 1")
	}

	cfg.Site.Title = defaultString(cfg.Site.Title, defaultSiteTitle)
	cfg.Site.Description = defaultString(cfg.Site.Description, defaultSiteDesc)

	// [safe_site] is optional — nil means single-site behavior. When
	// present, all three fields are required: hosts without the rest
	// (or the rest without hosts) is half a site; and an empty
	// allowed_networks would silently reduce the safe site to
	// safety='safe' rows only, which is almost certainly a config
	// mistake rather than an intent. loadConfig failing here also
	// fails a SIGHUP reload, keeping the running config untouched.
	if cfg.SafeSite != nil {
		if len(cfg.SafeSite.Hosts) == 0 {
			return cfg, fmt.Errorf("safe_site.hosts is required when [safe_site] is set")
		}
		if cfg.SafeSite.BaseURL == "" {
			return cfg, fmt.Errorf("safe_site.base_url is required when [safe_site] is set")
		}
		if len(cfg.SafeSite.AllowedNetworks) == 0 {
			return cfg, fmt.Errorf("safe_site.allowed_networks is required when [safe_site] is set")
		}
	}

	// [reactions]: absent section = built-in default set (Enabled
	// carries over as true); enabled=false disables the feature while
	// keeping stored rows; a present non-empty list is used verbatim.
	// This runs before the SafeSite block's return paths can skip it.
	if err := cfg.Reactions.normalize(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func defaultString(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

// defaultReactionEmojis is the built-in preset for an IRC-adjacent
// AI-art gallery — a robust spread (owner request, Oct 2026: the
// picker popup handles a large grid, so the default fills the whole
// cap): quick verdicts (fire/poop), meme flavor (skull/clown/eyes),
// engagement asks (pleading/pray), and art-appropriate ones
// (sparkles/art). Order is the picker-grid and tie-break order.
var defaultReactionEmojis = []ReactionEmoji{
	{Name: "fire", Glyph: "\U0001F525"},     // 🔥
	{Name: "laugh", Glyph: "\U0001F602"},    // 😂
	{Name: "skull", Glyph: "\U0001F480"},    // 💀
	{Name: "poop", Glyph: "\U0001F4A9"},     // 💩
	{Name: "eyes", Glyph: "\U0001F440"},     // 👀
	{Name: "clown", Glyph: "\U0001F921"},    // 🤡
	{Name: "wow", Glyph: "\U0001F62E"},      // 😮
	{Name: "thinking", Glyph: "\U0001F914"}, // 🤔
	{Name: "pleading", Glyph: "\U0001F97A"}, // 🥺
	{Name: "pray", Glyph: "\U0001F64F"},     // 🙏
	{Name: "sparkles", Glyph: "\U00002728"}, // ✨
	{Name: "art", Glyph: "\U0001F3A8"},      // 🎨
}

// reactionsEnabled reports whether the reaction feature is on: the
// absent key (nil pointer) is the default TRUE.
func (rc ReactionConfig) reactionsEnabled() bool {
	return rc.Enabled == nil || *rc.Enabled
}

// normalize validates a loaded preset and fills the default set when
// none was configured. Runs inside loadConfig, so an invalid preset
// fails startup AND a SIGHUP reload (running config stays live).
func (rc *ReactionConfig) normalize() error {
	if !rc.reactionsEnabled() {
		// Disabled: drop any configured list too, so EVERY surface
		// (endpoints, details row, card badges, glyph embed) agrees
		// without each one having to remember the flag. Rows on disk
		// are untouched.
		rc.Emojis = nil
		return nil
	}
	if len(rc.Emojis) == 0 {
		rc.Emojis = append([]ReactionEmoji(nil), defaultReactionEmojis...)
		return nil
	}
	if len(rc.Emojis) > maxReactionEmojis {
		return fmt.Errorf("reactions.emoji: at most %d entries, got %d", maxReactionEmojis, len(rc.Emojis))
	}
	seen := make(map[string]bool, len(rc.Emojis))
	for _, e := range rc.Emojis {
		if !validReactionName(e.Name) {
			return fmt.Errorf("reactions.emoji name %q: must be 1-24 chars of [a-z0-9_] (it rides the toggle URL)", e.Name)
		}
		if !utf8.ValidString(e.Glyph) || e.Glyph == "" || utf8.RuneCountInString(e.Glyph) > 16 {
			return fmt.Errorf("reactions.emoji %q: glyph must be 1-16 valid UTF-8 runes", e.Name)
		}
		if seen[e.Name] {
			return fmt.Errorf("reactions.emoji name %q: duplicated", e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}

// validReactionName pins the stored key shape: lowercase ASCII
// letters, digits, underscore, 1-24 chars — and NOT all digits: JS
// objects iterate integer-like keys (pure digit strings) in ascending
// numeric order BEFORE insertion order, which would silently break
// the client's configured-order badge tie-break parity with the
// server's ordered glyph embed. Path-safe by construction (the
// toggle URL embeds it) and impossible to confuse with an image id
// (which is exactly 7 chars of [0-9A-Za-z] — case differs, but more
// to the point these live under /{id}/react/{name}, a distinct path
// shape).
func validReactionName(s string) bool {
	if len(s) < 1 || len(s) > 24 {
		return false
	}
	hasNonDigit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
		if c < '0' || c > '9' {
			hasNonDigit = true
		}
	}
	return hasNonDigit
}

// reactionByName looks one preset entry up (config generation's
// snapshot — callers hold a Config value).
func (rc ReactionConfig) reactionByName(name string) (ReactionEmoji, bool) {
	for _, e := range rc.Emojis {
		if e.Name == name {
			return e, true
		}
	}
	return ReactionEmoji{}, false
}

// resolvePath joins a relative path onto baseDir (the binary directory),
// mirroring img-mcp's path resolution. Empty input yields empty output.
func resolvePath(baseDir, path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

// reloadConfigFromFile loads a fresh config and freezes every non-reloadable
// section at its current (startup) value, warning when the file tried to
// change one. Reloadable set: site.*, safe_site.*, thumbnails.* (except
// workers), search.*, upload.* (both fields are read per request —
// handleUpload re-reads max_bytes, the limiter retunes rate_per_minute).
// Same semantics as img-mcp's reload: an absent [safe_site] in the file
// disables the safe site without a restart, and an invalid one fails the
// whole reload (current config stays live).
func reloadConfigFromFile(configFile string, current Config) (Config, []string, error) {
	newCfg, err := loadConfig(configFile)
	if err != nil {
		return current, nil, err
	}

	warnings := compareNonReloadable(current, newCfg)

	// Non-reloadable sections stay frozen at their startup values; the
	// current copies already carry resolved paths.
	newCfg.Server = current.Server
	newCfg.Database = current.Database
	newCfg.Storage = current.Storage
	newCfg.Auth = current.Auth
	newCfg.Thumbnails.Workers = current.Thumbnails.Workers

	return newCfg, warnings, nil
}

func compareNonReloadable(current, newCfg Config) []string {
	var warnings []string

	if current.Server.Name != newCfg.Server.Name {
		warnings = append(warnings, fmt.Sprintf("server.name changed from %q to %q: requires restart", current.Server.Name, newCfg.Server.Name))
	}
	if current.Server.Addr != newCfg.Server.Addr {
		warnings = append(warnings, fmt.Sprintf("server.addr changed from %q to %q: requires restart", current.Server.Addr, newCfg.Server.Addr))
	}
	if current.Server.BaseURL != newCfg.Server.BaseURL {
		warnings = append(warnings, fmt.Sprintf("server.base_url changed from %q to %q: requires restart", current.Server.BaseURL, newCfg.Server.BaseURL))
	}
	if current.Database.Path != newCfg.Database.Path {
		warnings = append(warnings, fmt.Sprintf("database.path changed from %q to %q: requires restart", current.Database.Path, newCfg.Database.Path))
	}
	if current.Storage.Path != newCfg.Storage.Path {
		warnings = append(warnings, fmt.Sprintf("storage.path changed from %q to %q: requires restart", current.Storage.Path, newCfg.Storage.Path))
	}
	if current.Storage.ThumbsPath != newCfg.Storage.ThumbsPath {
		warnings = append(warnings, fmt.Sprintf("storage.thumbs_path changed from %q to %q: requires restart", current.Storage.ThumbsPath, newCfg.Storage.ThumbsPath))
	}
	if current.Auth.APIKey != newCfg.Auth.APIKey {
		warnings = append(warnings, "auth.api_key changed: requires restart")
	}
	if current.Thumbnails.Workers != newCfg.Thumbnails.Workers {
		warnings = append(warnings, fmt.Sprintf("thumbnails.workers changed from %d to %d: requires restart", current.Thumbnails.Workers, newCfg.Thumbnails.Workers))
	}

	return warnings
}
