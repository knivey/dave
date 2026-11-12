package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config mirrors the reference block in example.toml (repo convention).
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Render   RenderConfig   `toml:"render"`
	Database DatabaseConfig `toml:"database"`
}

type ServerConfig struct {
	Addr    string `toml:"addr"`
	BaseURL string `toml:"base_url"`
	// TrustedProxies lists direct peers whose X-Forwarded-For may be
	// trusted when keying the SSE per-IP cap (events.go). Absent
	// (nil) defaults to loopback only — the local-nginx deployment;
	// an explicitly empty list never trusts XFF (RemoteAddr always).
	TrustedProxies []string `toml:"trusted_proxies"`
}

// RenderConfig controls transcript rendering.
//
// hide_system_prompts removes role=system rows (the bot's instruction
// prompt, guidance injections) from transcripts AND 404s their raw
// endpoints — hiding the display while leaving raw readable would be
// pointless. Compaction summaries are system rows too, but they are
// conversation-derived rather than instructions: their content keeps
// rendering through the compaction divider's inline summary (the row
// itself is filtered like any other system row). Default false.
//
// bbcode (default TRUE, pointer so false is explicit) translates the
// model's BBCode tags to styled spans for ASSISTANT-side content —
// the same translation dave applies at IRC emission (the site's
// translator is a verbatim port), so the web shows what IRC saw.
// bbcode_styles gates the [b] [i] [u] [s] toggle half exactly like
// dave's knob (default false: single-letter tags are bracket-ambiguous
// with prose — `a[i]` indexing, [1] citations — so they stay verbatim
// unless turned on). Compaction summaries are NOT translated (summarizer
// prose). Set both to match the fleet's command configs.
type RenderConfig struct {
	HideSystemPrompts bool  `toml:"hide_system_prompts"`
	BBCode            *bool `toml:"bbcode"`
	BBCodeStyles      bool  `toml:"bbcode_styles"`
}

// bbcodeOn resolves the bbcode knob (nil = true, the fix-the-complaint
// default; explicit false opts out).
func (c *RenderConfig) bbcodeOn() bool {
	return c.BBCode == nil || *c.BBCode
}

// viewOptsFor collapses the render config into the view-construction
// toggles.
func (c *RenderConfig) viewOptsFor() viewOpts {
	return viewOpts{BBCode: c.bbcodeOn(), BBCodeStyles: c.BBCodeStyles}
}

type DatabaseConfig struct {
	Driver string `toml:"driver"` // "sqlite" | "postgres"
	Path   string `toml:"path"`   // sqlite
	DSN    string `toml:"dsn"`    // postgres
}

func (c *Config) setDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8060"
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.Path == "" {
		c.Database.Path = "data/dave.db"
	}
	// Absent → loopback-only default; an explicitly empty
	// `trusted_proxies = []` decodes as an empty non-nil slice and is
	// preserved (never trust XFF).
	if c.Server.TrustedProxies == nil {
		c.Server.TrustedProxies = []string{"127.0.0.1", "::1"}
	}
}

func loadConfig(path string) (*Config, error) {
	var cfg Config
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", path, err)
		}
	}
	cfg.setDefaults()
	return &cfg, nil
}

// resolvePath resolves a config-relative path against the binary dir
// (imgsite/main.go precedent: config and data live next to the binary).
func resolvePath(binaryDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(binaryDir, p)
}
