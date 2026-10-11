package main

// Presets: shared base configs (config/presets.toml) that commands in
// chats.toml, completions.toml, and generators.toml inherit from via
// `preset = "name"`. A preset may itself reference another preset
// (layering); expansion is base-first and command keys win last.
//
// DESIGN NOTE — why the merge happens at the raw TOML layer: merging
// decoded structs cannot distinguish "field absent" (inherit from the
// preset) from "field explicitly set to its zero value" (override the
// preset) for bools/ints/strings — `streaming = false` on a command
// whose preset sets `streaming = true` would silently lose. Decoding
// each file into map[string]map[string]any first, merging the raw
// key-value tables, and only then decoding into the typed structs keeps
// the distinction exact. After the merge, the existing decode →
// validateAIConfig → service cascade → /reload paths all run unchanged
// on the merged tables; presets are invisible downstream.
//
// Merge semantics: scalars and arrays are replaced wholesale by the more
// derived table (mcps / aliases / stop never concatenate — an override
// means "this value"); nested maps (extra_body, chat_template_kwargs,
// generator [name.log]) deep-merge per key so a command can tweak one
// sub-key of a preset's map without re-declaring the rest.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// loadPresetsFile reads config-dir presets.toml into raw tables. A
// missing file is not an error (empty map), matching the other optional
// config files. The whole preset graph is validated eagerly: every
// preset's chain must resolve, so unknown references and cycles fail
// the load even when no command uses them yet — a broken preset is a
// config error, not dead text waiting to trap the next opt-in.
func loadPresetsFile(dir string) (map[string]map[string]any, error) {
	presets := make(map[string]map[string]any)
	path := filepath.Join(dir, "presets.toml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return presets, nil
	}
	if _, err := toml.DecodeFile(path, &presets); err != nil {
		return presets, fmt.Errorf("loading %s: %w", path, err)
	}
	for name := range presets {
		if _, err := resolvePresetChain(name, presets); err != nil {
			return presets, fmt.Errorf("presets.toml: %w", err)
		}
	}
	return presets, nil
}

// resolvePresetChain walks a preset's `preset` references and returns
// the tables to merge, BASE-FIRST (the most derived table last, so
// merging in order yields correct override precedence). The loop guard
// is membership in the walked chain: any revisit — including a preset
// referencing itself — is a cycle and errors naming the whole chain, so
// no configuration can make expansion recurse forever.
func resolvePresetChain(name string, presets map[string]map[string]any) ([]map[string]any, error) {
	var chain []string
	cur := name
	for {
		table, ok := presets[cur]
		if !ok {
			if len(chain) == 0 {
				return nil, fmt.Errorf("preset %q is undefined", name)
			}
			return nil, fmt.Errorf("preset %q is undefined (chain: %s)", cur, strings.Join(chain, " -> "))
		}
		for _, walked := range chain {
			if walked == cur {
				return nil, fmt.Errorf("preset cycle detected: %s -> %s", strings.Join(chain, " -> "), cur)
			}
		}
		chain = append(chain, cur)
		ref, hasRef := table["preset"]
		if !hasRef {
			break
		}
		refName, ok := ref.(string)
		if !ok || refName == "" {
			return nil, fmt.Errorf("preset %q: preset must be a non-empty string", cur)
		}
		cur = refName
	}
	tables := make([]map[string]any, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		tables = append(tables, presets[chain[i]])
	}
	return tables, nil
}

// mergeTables returns a NEW map with overlay's keys layered over base
// (never mutating either — preset tables are shared across commands).
// Nested maps deep-merge; everything else is replaced wholesale.
func mergeTables(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		if bm, ok := out[k].(map[string]any); ok {
			if om, ok := v.(map[string]any); ok {
				out[k] = mergeTables(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// loadCommandFileWithPresets is loadCommandFile for the AIConfig-shaped
// command maps (chats, completions, generators): decode the file into
// raw tables, expand each command's `preset` reference, then decode the
// merged tables into dest.
func loadCommandFileWithPresets(path string, presets map[string]map[string]any, dest any) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	raw := make(map[string]map[string]any)
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return err
	}
	for name, table := range raw {
		ref, hasRef := table["preset"]
		if !hasRef {
			continue
		}
		refName, ok := ref.(string)
		if !ok || refName == "" {
			return fmt.Errorf("%s: command %q: preset must be a non-empty string", filepath.Base(path), name)
		}
		tables, err := resolvePresetChain(refName, presets)
		if err != nil {
			return fmt.Errorf("%s: command %q: %w", filepath.Base(path), name, err)
		}
		delete(table, "preset") // consumed; never decode into dest
		merged := map[string]any{}
		for _, t := range tables {
			merged = mergeTables(merged, t)
		}
		merged = mergeTables(merged, table) // command's own keys win last
		raw[name] = merged
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return fmt.Errorf("%s: re-encoding merged tables: %w", filepath.Base(path), err)
	}
	if _, err := toml.Decode(buf.String(), dest); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}
