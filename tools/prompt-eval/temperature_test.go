package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTemperatureLabels(t *testing.T) {
	assert.Equal(t, defaultTemperature, temperatureLabel(0))
	assert.Equal(t, "0.7", temperatureLabel(0.7))
	assert.Equal(t, "1.5", temperatureLabel(1.5))
	assert.Equal(t, "1", temperatureLabel(1))
	assert.Equal(t, "0.35", temperatureValue(0.35))

	assert.NoError(t, validateTemperature(0))
	assert.NoError(t, validateTemperature(0.7))
	for _, bad := range []float64{-0.1, -1} {
		assert.ErrorContainsf(t, validateTemperature(bad), "-temperature", "%v must be rejected", bad)
	}
}

// TestTemperatureRidesRequestOnlyWhenSet pins dave's convention on the
// wire: a value > 0 sends "temperature" on every request, 0 omits the key
// entirely (server-default sampling). The field must never appear as an
// explicit 0.
func TestTemperatureRidesRequestOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		temp float64
		want string // "" = the key must be absent
	}{
		{temp: 0, want: ""},
		{temp: 0.7, want: "0.7"},
		{temp: 1.5, want: "1.5"},
		{temp: 1, want: "1"},
	} {
		t.Run(fmt.Sprint(tc.temp), func(t *testing.T) {
			cfg := runConfig{model: "m", tools: advertisedTools, deliveryRole: "system", temperature: tc.temp}
			s := newSession(cfg, "SYS PROMPT", nil, nil, nil)
			for _, forceFinal := range []bool{false, true} {
				body, err := json.Marshal(s.buildRequest(forceFinal))
				require.NoError(t, err)
				var m map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(body, &m))
				if tc.want == "" {
					_, ok := m["temperature"]
					assert.Falsef(t, ok, "temperature must be omitted at 0 (forceFinal=%v), got body %s", forceFinal, body)
				} else {
					require.Containsf(t, m, "temperature", "temperature must ride the request (forceFinal=%v)", forceFinal)
					assert.JSONEqf(t, tc.want, string(m["temperature"]), "forceFinal=%v", forceFinal)
				}
			}
		})
	}
}

// TestTemperatureDoesNotTouchDeterminismStreams pins the constraint that
// the temperature axis rides the REQUEST only: with the same seed, temp 0
// and temp 0.7 sessions must fabricate identical job ids — the axis never
// leaks into the seeded rng keys.
func TestTemperatureDoesNotTouchDeterminismStreams(t *testing.T) {
	firstJobID := func(temp float64) string {
		sc := &script{responses: []string{
			cannedResp("", "planning", call("call_1", "enhance_and_generate_async", `{"prompt":"group portrait"}`)),
			cannedResp("one moment...", ""),
			cannedResp("behold!", ""),
			cannedResp("a corrected likeness is being prepared", ""),
			cannedResp("done", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "testv", "imageflow")
		s.cfg.temperature = temp
		require.NoError(t, s.runImageflow(1, phraseOffset(1, "testv", "imageflow")))
		reqs := decodeRequests(t, sc.requests)
		return jobIDOf(t, lastMsg(t, reqs[1]).Content)
	}
	assert.Equal(t, firstJobID(0), firstJobID(0.7),
		"temperature cells with the same seed must fabricate identical job ids")
}

// TestRunOneTemperaturePath pins the output path rule: the variant dir
// gains "@<temp>" only when the axis is set, composing after a
// tool-variant suffix; the row records the numeric label ("default" when
// unset) and no sibling cells appear.
func TestRunOneTemperaturePath(t *testing.T) {
	for _, tc := range []struct {
		temp      float64
		toolVar   string
		wantDir   string
		wantLabel string
	}{
		{temp: 0, toolVar: "", wantDir: "testv", wantLabel: "default"}, // the axis absent
		{temp: 0.7, toolVar: "", wantDir: "testv@0.7", wantLabel: "0.7"},
		{temp: 1.5, toolVar: "waitinstr", wantDir: "testv+waitinstr@1.5", wantLabel: "1.5"},
	} {
		t.Run(tc.wantDir, func(t *testing.T) {
			outdir := t.TempDir()
			sc := &script{responses: []string{ // minimal no-tool imageflow shape
				cannedResp("i shall simply describe it...", ""),
				cannedResp("as delivered", ""),
				cannedResp("hmm, no tool again", ""),
				cannedResp("very well", ""),
			}}
			cfg := runConfig{
				model:           "test-model",
				tools:           advertisedTools,
				seed:            1,
				deliveryRole:    "system",
				toolVariantName: tc.toolVar,
				temperature:     tc.temp,
			}
			row, err := runOne(loadedVariant{name: "testv", text: "SYS PROMPT"}, scenarioImageflow, 1, cfg, sc.responder(), outdir)
			require.NoError(t, err)
			require.NotNil(t, row)

			assert.FileExists(t, filepath.Join(outdir, tc.wantDir, "imageflow-system", "run-01.jsonl"),
				"the variant dir follows the <prompt>[+<tool>][@<temp>] rule")
			entries, err := os.ReadDir(outdir)
			require.NoError(t, err)
			require.Len(t, entries, 1, "exactly one variant directory per cell")
			assert.Equal(t, tc.wantDir, entries[0].Name())

			assert.Equal(t, "testv", row.Variant, "the CSV variant column stays the bare prompt variant")
			assert.Equal(t, tc.wantLabel, row.Temperature, "the CSV temperature column")
			assert.Equal(t, 0, row.Retries, "no retries in a clean session")
		})
	}
}

// TestRunRejectsNegativeTemperature pins the startup wiring: a negative
// -temperature fails run() before any directory is created or any request
// is made. flag state is swapped for a fresh FlagSet because run()
// registers on the package CommandLine.
func TestRunRejectsNegativeTemperature(t *testing.T) {
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	oldUsage := flag.Usage
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
		flag.Usage = oldUsage
	})
	outdir := filepath.Join(t.TempDir(), "out")
	flag.CommandLine = flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = []string{"prompt-eval", "-temperature", "-0.5", "-outdir", outdir, "-runs", "1"}

	err := run()
	require.ErrorContains(t, err, "-temperature")
	require.ErrorContains(t, err, "-0.5")
	assert.NoDirExists(t, outdir, "validation must fail before the outdir is created")
}
