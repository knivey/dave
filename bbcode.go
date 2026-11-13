package main

// BBCode gating (Nov 2026 relocation): the translator itself now lives in
// MarkdownToIRC/bbcode.go (package markdowntoirc, COLOR-ONLY — style
// toggles were deleted, owner ruling 2026-11-13). This file keeps only
// the root-side resolvers: which configs get translation, and the warn
// callback adapter that hands the package a logger.

import (
	logxi "github.com/mgutz/logxi/v1"
)

// bbcodeEnabled resolves whether BBCode is on for this config (cascade
// command > service > false, materialized in ApplyDefaults; nil check is
// defensive for hand-built configs). Gates the markdown-renderer BBCode
// extension and the plain-mode byte translator (see MarkdownToIRC/bbcode.go).
func bbcodeEnabled(cfg AIConfig) bool {
	return cfg.BBCode != nil && *cfg.BBCode
}

// bbcodeStylesEnabled resolves the style-toggle half of the OLD translator
// split ([b] [i] [u] [s] behind bbcode_styles). ORPHANED by the toggle
// deletion: no production call site reads it anymore — the translator is
// color-only regardless. Kept compiling only because the config-field
// tests (TestAIConfigApplyDefaults_BBCodeStyles, TestLoadConfigDirBBCodeStyles)
// still exercise the cascade; the `bbcode_styles` CONFIG FIELD and this
// resolver are deleted together with those tests in the follow-up change
// (spec §3, docs/superpowers/specs/2026-11-13-bbcode-in-markdown-design.md).
func bbcodeStylesEnabled(cfg AIConfig) bool {
	return cfg.BBCodeStyles != nil && *cfg.BBCodeStyles
}

// bbcodeWarnFn adapts a logxi logger to the translator's warn callback;
// nil logger (and nil result) is silent.
func bbcodeWarnFn(l logxi.Logger) func(string) {
	if l == nil {
		return nil
	}
	return func(msg string) { l.Warn(msg) }
}
