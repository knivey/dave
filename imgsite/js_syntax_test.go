package main

// js_syntax_test.go — a compile guard for the hand-rolled ES modules
// in web/. The Go suite was green for a whole commit while image.js
// carried a SyntaxError (an edit left an orphaned handler tail), which
// killed EVERY details-page enhancement — the module import rejects,
// so app.js's boot() never ran and the owner saw only the no-JS
// markup. Nothing in the Go toolchain parses JS, so this test borrows
// deno when it is on PATH and skips otherwise (the repo has no JS
// toolchain dependency by policy; this is an opportunistic tripwire,
// not a build requirement). vendor/ is excluded — picmo.js is a
// minified third-party artifact, not ours to check.

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSSyntax(t *testing.T) {
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("deno not on PATH — JS syntax unchecked (install deno to enable this guard)")
	}

	matches, err := filepath.Glob("web/*.js")
	require.NoError(t, err)
	require.NotEmpty(t, matches, "web/ carries the page modules")

	args := append([]string{"check", "--quiet"}, matches...)
	cmd := exec.Command(deno, args...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "a web/*.js module fails deno check — every page enhancement is dead:\n%s", out)
}
