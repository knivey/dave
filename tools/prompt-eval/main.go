// Command prompt-eval is a model-behavior A/B harness for the dave IRC
// bot's system prompts: it replays scripted sessions against a live
// llama-server with different FINAL rendered system prompts, answers every
// tool call from a fabrication table (no MCP servers, no side effects),
// and writes api-log-shaped JSONL plus a run-index.csv so prompt effects
// (thinking length, delivery-marker fabrication, stability) can be
// compared offline.
//
// The wire shape mirrors what dave sends in production: non-streaming
// chat-completions with the prod eunuch tool advertisement,
// chat_template_kwargs, and reasoning_effort. Everything is sequential,
// with -delay slept between HTTP requests — the server is shared with a
// live bot. The tool never touches anything outside -outdir.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL  = "http://192.168.100.45:8000/v1"
	defaultModel    = "nohurry/gemma-4-26B-A4B-it-heretic-GUFF:Q5_K_S"
	defaultOutdir   = "results"
	defaultScenList = "imageflow,prompts"

	scenarioImageflow = "imageflow"
	scenarioPrompts   = "prompts"
	scenarioResearch  = "research"
)

// runConfig carries the invocation-level knobs every session shares.
type runConfig struct {
	baseURL         string
	model           string
	apiKey          string
	tools           []toolDef
	reasoningEffort string
	kwargs          map[string]any
	delay           time.Duration
	seed            int64

	// deliveryRole is the -delivery-role axis: the wire role of the
	// imageflow delivery-marker injections (dave's injection_role knob).
	// Validated by validateDeliveryRole before anything runs.
	deliveryRole string

	// toolVariantName is the -tool-variant axis: the sweep name of the
	// tool-description overrides already applied to cfg.tools ("" = no
	// variant, the baseline advertisement). Validated by loadToolVariants
	// before anything runs; labeled "default" in run-index.csv.
	toolVariantName string

	// temperature is the -temperature axis: sent on every request when
	// > 0 (dave's convention — 0 omits the field and samples at the
	// server default). Negative values are rejected at startup. The axis
	// also suffixes the variant directory ("@0.7") so temperature cells
	// never collide with default-invocation output.
	temperature float64

	// retries / retryWait shape the transient-failure policy: a request
	// that fails with HTTP >= 500 or a transport-level error is re-sent
	// (identical body) up to retries times — the router in front of the
	// server intermittently drops connections, and one lost request used
	// to kill a whole scripted session. retryWait is the exponential
	// backoff base, doubled per attempt.
	retries   int
	retryWait time.Duration
}

func main() {
	flag.Usage = usage
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "prompt-eval:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: prompt-eval [-baseurl URL] [-model NAME] [-variant name=path] [-tool-variant name=path]")
	fmt.Fprintln(os.Stderr, "                  [-temperature F] [-delivery-role system|user|developer] [-runs N] [-delay D]")
	fmt.Fprintln(os.Stderr, "                  [-outdir DIR] [-scenarios imageflow,prompts[,research]] [-reasoning-effort low]")
	fmt.Fprintln(os.Stderr, "                  [-chat-template-kwargs JSON] [-api-key KEY] [-seed N] [-retries N] [-retry-wait D]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Runs scripted bot sessions against a live llama-server with different system-prompt")
	fmt.Fprintln(os.Stderr, "variants and tool-description variants, fabricates every tool result, and writes")
	fmt.Fprintln(os.Stderr, "api-log-shaped JSONL under")
	fmt.Fprintln(os.Stderr, "<outdir>/<variant>[+<tool-variant>][@<temp>]/<scenario>-<delivery-role>/run-NN.jsonl plus a")
	fmt.Fprintln(os.Stderr, "<outdir>/run-index.csv summary.")
	fmt.Fprintln(os.Stderr, "All requests are sequential with -delay between them (the server is shared with a")
	fmt.Fprintln(os.Stderr, "live bot). Transient failures (HTTP >= 500 or transport errors) are retried")
	fmt.Fprintln(os.Stderr, "-retries times per request with exponential backoff. The tool never touches")
	fmt.Fprintln(os.Stderr, "anything outside -outdir.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Flags:")
	flag.PrintDefaults()
}

func run() error {
	var variantFlags variantList
	var toolVariantFlags toolVariantList
	baseURL := flag.String("baseurl", defaultBaseURL, "llama-server base URL (OpenAI-compatible, up to and including /v1)")
	model := flag.String("model", defaultModel, "model name as the router knows it")
	flag.Var(&variantFlags, "variant", "variant as name=path to a FINAL rendered system prompt (repeatable; default: bundled none/dial/broken)")
	flag.Var(&toolVariantFlags, "tool-variant", "tool-description variant as name=path to a JSON object of advertised tool name → replacement description (repeatable; default: none)")
	runs := flag.Int("runs", 10, "sessions per scenario per variant")
	delay := flag.Duration("delay", 750*time.Millisecond, "sleep between HTTP requests (the server is shared with a live bot)")
	outdir := flag.String("outdir", defaultOutdir, "output directory for JSONL runs and run-index.csv")
	scenarioFlag := flag.String("scenarios", defaultScenList, "comma-separated scenarios: imageflow,prompts,research")
	effort := flag.String("reasoning-effort", "low", "reasoning_effort sent on every request (empty omits)")
	kwargsFlag := flag.String("chat-template-kwargs", `{"preserve_thinking":true}`, "JSON object sent as chat_template_kwargs (empty omits)")
	apiKey := flag.String("api-key", "", "optional Authorization: Bearer key")
	seed := flag.Int64("seed", 1, "determinism seed for fabricated ids/urls and phrase rotation")
	deliveryRole := flag.String("delivery-role", "system", "wire role of the imageflow async-delivery injections (system, user, or developer)")
	temperature := flag.Float64("temperature", 0, "sampling temperature sent on every request; 0 omits the field (server default), negative is invalid")
	retries := flag.Int("retries", 3, "per-request retries on transient failures (HTTP >= 500 or transport errors); 0 disables")
	retryWait := flag.Duration("retry-wait", 2*time.Second, "base backoff between retries, doubled per attempt (2s, 4s, 8s, ...)")
	flag.Parse() // after registration; main only installs the usage hook

	if *runs < 1 {
		return fmt.Errorf("-runs must be at least 1, got %d", *runs)
	}
	if err := validateDeliveryRole(*deliveryRole); err != nil {
		return err
	}
	if err := validateTemperature(*temperature); err != nil {
		return err
	}
	if *retries < 0 {
		return fmt.Errorf("-retries must be >= 0, got %d", *retries)
	}
	if *retryWait < 0 {
		return fmt.Errorf("-retry-wait must be >= 0, got %s", *retryWait)
	}
	var kwargs map[string]any
	if s := strings.TrimSpace(*kwargsFlag); s != "" {
		if err := json.Unmarshal([]byte(s), &kwargs); err != nil {
			return fmt.Errorf("-chat-template-kwargs: %w", err)
		}
	}
	scenarios, err := parseScenarios(*scenarioFlag)
	if err != nil {
		return err
	}
	variants, err := loadVariants(variantFlags)
	if err != nil {
		return err
	}
	// Validated (unknown tool names, empty descriptions, malformed JSON)
	// before the outdir is created — a typo'd experiment must fail loudly
	// at startup, never silently no-op into the sweep.
	toolVariants, err := loadToolVariants(toolVariantFlags)
	if err != nil {
		return err
	}
	cfg := runConfig{
		baseURL:         *baseURL,
		model:           *model,
		apiKey:          *apiKey,
		tools:           advertisedTools,
		reasoningEffort: *effort,
		kwargs:          kwargs,
		delay:           *delay,
		seed:            *seed,
		deliveryRole:    *deliveryRole,
		temperature:     *temperature,
		retries:         *retries,
		retryWait:       *retryWait,
	}

	if err := os.MkdirAll(*outdir, 0o755); err != nil {
		return fmt.Errorf("creating outdir: %w", err)
	}
	idxFile, err := os.Create(filepath.Join(*outdir, "run-index.csv"))
	if err != nil {
		return fmt.Errorf("creating run-index.csv: %w", err)
	}
	defer idxFile.Close()
	idx := newRunIndex(idxFile)

	respond := newLiveResponder(cfg)
	failures := 0
	total := len(variants) * len(toolVariants) * len(scenarios) * *runs
	for _, v := range variants {
		for _, tv := range toolVariants {
			// One config per cell: the tools field carries this cell's
			// overridden advertisement; toolVariantName rides along for
			// the output path and the CSV row.
			cellCfg := cfg
			cellCfg.toolVariantName = tv.name
			cellCfg.tools = applyToolVariant(advertisedTools, tv.overrides)
			for _, sc := range scenarios {
				for run := 1; run <= *runs; run++ {
					row, err := runOne(v, sc, run, cellCfg, respond, *outdir)
					if err != nil {
						fmt.Fprintf(os.Stderr, "prompt-eval: %s/%s-%s run %d: %v\n",
							variantDirName(v.name, tv.name, cfg.temperature), sc, cellCfg.deliveryRole, run, err)
						failures++
					}
					if row != nil {
						if err := idx.add(*row); err != nil {
							return fmt.Errorf("writing run-index.csv: %w", err)
						}
					}
				}
			}
		}
	}
	if err := idx.close(); err != nil {
		return fmt.Errorf("closing run-index.csv: %w", err)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d session(s) failed", failures, total)
	}
	return nil
}

func parseScenarios(flagVal string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, name := range strings.Split(flagVal, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		switch name {
		case scenarioImageflow, scenarioPrompts, scenarioResearch:
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		default:
			return nil, fmt.Errorf("unknown scenario %q (want imageflow, prompts, or research)", name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-scenarios selected nothing")
	}
	return out, nil
}

// validateDeliveryRole enforces the -delivery-role axis at startup: the
// three roles dave's injection_role knob accepts, nothing else (exact
// match; -delivery-role "" is as invalid as a typo).
func validateDeliveryRole(role string) error {
	switch role {
	case "system", "user", "developer":
		return nil
	}
	return fmt.Errorf("-delivery-role must be system, user, or developer, got %q", role)
}

// runOne executes one scripted session and writes its JSONL log. A nil row
// means the session never started (its counts are meaningless); a non-nil
// row is returned even when the session failed mid-run so partial data
// still lands in run-index.csv.
func runOne(v loadedVariant, scenario string, run int, cfg runConfig, respond responder, outdir string) (*indexRow, error) {
	// The scenario dir always carries the -delivery-role suffix (even the
	// default) so separate invocations can never collide on run-NN.jsonl.
	// The variant dir carries the -tool-variant axis the same way ("+name"
	// when active, bare when not). The rng/phrase keys below deliberately
	// stay on the BARE scenario and variant: role A/B and description A/B
	// runs with the same seed fabricate identical job ids and phrasings, so
	// the only variable between cells is the axis itself.
	scenDirName := scenario + "-" + cfg.deliveryRole
	varDir := variantDirName(v.name, cfg.toolVariantName, cfg.temperature)
	scenDir := filepath.Join(outdir, varDir, scenDirName)
	if err := os.MkdirAll(scenDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", scenDir, err)
	}
	path := filepath.Join(scenDir, fmt.Sprintf("run-%02d.jsonl", run))
	w, err := newJSONLWriter(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer w.Close()

	rng := seededRng(cfg.seed, v.name, scenario, strconv.Itoa(run))
	offset := phraseOffset(cfg.seed, v.name, scenario)
	s := newSession(cfg, v.text, respond, w, rng)

	var runErr error
	switch scenario {
	case scenarioImageflow:
		runErr = s.runImageflow(run, offset)
	case scenarioPrompts:
		runErr = s.runPrompts()
	case scenarioResearch:
		runErr = s.runResearch()
	default:
		runErr = fmt.Errorf("unknown scenario %q", scenario)
	}

	row := &indexRow{
		Variant:        v.name,
		ToolVariant:    toolVariantLabel(cfg.toolVariantName),
		Temperature:    temperatureLabel(cfg.temperature),
		Retries:        s.retryCount,
		Scenario:       scenario,
		DeliveryRole:   cfg.deliveryRole,
		Run:            run,
		Requests:       s.reqCount,
		Responses:      s.respCount,
		ReasoningChars: s.reasoningChars,
		Fabricated:     s.fabricated,
	}
	fmt.Printf("%s/%s run %d done: %d requests, reasoning %d chars, fabricated %v\n",
		varDir, scenDirName, run, s.reqCount, s.reasoningChars, s.fabricated)
	return row, runErr
}

// variantSpec is one unresolved -variant name=path flag.
type variantSpec struct {
	name string
	path string
}

// variantList is a repeatable -variant flag.
type variantList []variantSpec

func (v variantList) String() string {
	parts := make([]string, len(v))
	for i, s := range v {
		parts[i] = s.name + "=" + s.path
	}
	return strings.Join(parts, ", ")
}

func (v *variantList) Set(s string) error {
	name, path, ok := strings.Cut(s, "=")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("variant must be name=path, got %q", s)
	}
	*v = append(*v, variantSpec{name: name, path: path})
	return nil
}

// loadedVariant is a fully rendered system prompt under its sweep name.
type loadedVariant struct {
	name string
	text string
}

// loadVariants resolves the variant set: explicit -variant flags win; the
// default is the three shipped variants (none/dial/broken).
func loadVariants(flags variantList) ([]loadedVariant, error) {
	specs := flags
	if len(specs) == 0 {
		dir, err := defaultVariantDir()
		if err != nil {
			return nil, err
		}
		specs = []variantSpec{
			{name: "none", path: filepath.Join(dir, "none.txt")},
			{name: "dial", path: filepath.Join(dir, "dial.txt")},
			{name: "broken", path: filepath.Join(dir, "broken.txt")},
		}
	}
	out := make([]loadedVariant, 0, len(specs))
	seen := map[string]bool{}
	for _, sp := range specs {
		if seen[sp.name] {
			return nil, fmt.Errorf("duplicate variant name %q", sp.name)
		}
		seen[sp.name] = true
		b, err := os.ReadFile(sp.path)
		if err != nil {
			return nil, fmt.Errorf("variant %s: %w", sp.name, err)
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			return nil, fmt.Errorf("variant %s: %s is empty", sp.name, sp.path)
		}
		out = append(out, loadedVariant{name: sp.name, text: string(b)})
	}
	return out, nil
}

// defaultVariantDir finds the bundled variants/ directory: next to the
// binary first (the house pattern), then the two plausible cwd layouts
// (built-binary-from-repo-root and go-run-from-repo-root).
func defaultVariantDir() (string, error) {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "variants"))
	}
	candidates = append(candidates,
		filepath.Join("tools", "prompt-eval", "variants"),
		"variants",
	)
	for _, d := range candidates {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d, nil
		}
	}
	return "", fmt.Errorf("no -variant flags given and no bundled variants/ directory found (looked in: %s)", strings.Join(candidates, ", "))
}
