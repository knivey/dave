package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// defaultToolVariant is the run-index.csv label for a sweep cell with no
// tool-description overrides (the -tool-variant axis absent).
const defaultToolVariant = "default"

// toolVariantSpec is one unresolved -tool-variant name=path flag.
type toolVariantSpec struct {
	name string
	path string
}

// toolVariantList is a repeatable -tool-variant flag.
type toolVariantList []toolVariantSpec

func (v toolVariantList) String() string {
	parts := make([]string, len(v))
	for i, s := range v {
		parts[i] = s.name + "=" + s.path
	}
	return strings.Join(parts, ", ")
}

func (v *toolVariantList) Set(s string) error {
	name, path, ok := strings.Cut(s, "=")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("tool variant must be name=path, got %q", s)
	}
	*v = append(*v, toolVariantSpec{name: name, path: path})
	return nil
}

// loadedToolVariant is one resolved tool-description variant: a set of
// per-tool description overrides under its sweep name. name "" is the
// synthetic default cell (no overrides) loadToolVariants returns when no
// -tool-variant flags were given; the CSV labels it "default".
type loadedToolVariant struct {
	name      string
	overrides map[string]string
}

// loadToolVariants resolves the tool-variant axis: explicit -tool-variant
// flags become one cell each; no flags means the single synthetic default
// cell. Everything is validated here, loudly, BEFORE any output is written:
// a typo'd experiment must fail at startup, never silently no-op into a
// mislabeled cell that looks like real data.
func loadToolVariants(flags toolVariantList) ([]loadedToolVariant, error) {
	if len(flags) == 0 {
		return []loadedToolVariant{{}}, nil
	}
	advertised := map[string]bool{}
	for _, td := range advertisedTools {
		advertised[td.Function.Name] = true
	}
	out := make([]loadedToolVariant, 0, len(flags))
	seen := map[string]bool{}
	for _, sp := range flags {
		if seen[sp.name] {
			return nil, fmt.Errorf("duplicate tool variant name %q", sp.name)
		}
		seen[sp.name] = true
		if err := validateToolVariantName(sp.name); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(sp.path)
		if err != nil {
			return nil, fmt.Errorf("tool variant %s: %w", sp.name, err)
		}
		var overrides map[string]string
		if err := json.Unmarshal(b, &overrides); err != nil {
			return nil, fmt.Errorf("tool variant %s: parsing %s: %w", sp.name, sp.path, err)
		}
		if len(overrides) == 0 {
			return nil, fmt.Errorf("tool variant %s: %s contains no overrides", sp.name, sp.path)
		}
		// Sorted keys so an invalid file always reports the same first
		// offense, whatever map iteration order drew.
		keys := make([]string, 0, len(overrides))
		for k := range overrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !advertised[k] {
				return nil, fmt.Errorf("tool variant %s: unknown tool name %q in %s (must be one of the advertised tools)", sp.name, k, sp.path)
			}
			if strings.TrimSpace(overrides[k]) == "" {
				return nil, fmt.Errorf("tool variant %s: description for %q in %s is empty", sp.name, k, sp.path)
			}
		}
		out = append(out, loadedToolVariant{name: sp.name, overrides: overrides})
	}
	return out, nil
}

// validateToolVariantName guards the name that lands in the output path and
// the CSV: "default" is the reserved label for the no-variant cell (a user
// variant with that name would silently collide with it), and '/', '+', or
// '\' would corrupt the "<prompt>+<tool>" directory component.
func validateToolVariantName(name string) error {
	if name == defaultToolVariant {
		return fmt.Errorf("tool variant name %q is reserved for the no-variant cell", name)
	}
	if strings.ContainsAny(name, `/+\`) {
		return fmt.Errorf("tool variant name %q must not contain '/', '+', or '\\'", name)
	}
	return nil
}

// applyToolVariant returns the tool advertisement for one sweep cell: base
// unchanged when there are no overrides, otherwise a shallow copy with the
// named descriptions replaced. base itself (the package-global prod
// advertisement) is never mutated — every cell renders from the same
// original.
func applyToolVariant(base []toolDef, overrides map[string]string) []toolDef {
	if len(overrides) == 0 {
		return base
	}
	out := make([]toolDef, len(base))
	copy(out, base)
	for i := range out {
		if desc, ok := overrides[out[i].Function.Name]; ok {
			out[i].Function.Description = desc
		}
	}
	return out
}

// variantDirName builds the output-directory component for one sweep cell:
// the bare prompt-variant name normally, "<prompt>+<tool>" when a
// tool-description variant is active, "@<temp>" appended when the
// temperature axis is set — separate cells must never collide on
// run-NN.jsonl, and the unsuffixed name must stay exactly what a default
// invocation produces.
func variantDirName(promptVariant, toolVariant string, temperature float64) string {
	name := promptVariant
	if toolVariant != "" {
		name += "+" + toolVariant
	}
	if temperature > 0 {
		name += "@" + temperatureValue(temperature)
	}
	return name
}

// toolVariantLabel maps a cell's internal tool-variant name to its
// run-index.csv value: the zero value (no -tool-variant axis) reads
// "default".
func toolVariantLabel(name string) string {
	if name == "" {
		return defaultToolVariant
	}
	return name
}
