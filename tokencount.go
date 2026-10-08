package main

// Real-tokenizer token accounting for session payloads.
//
// WHY THIS EXISTS (owner's problem, Oct 2026): xAI/Grok reports roughly
// 2x the prompt_tokens for the same session content where OpenAI reports
// numbers consistent with the content size (cache accounted for). To tell
// a provider-side accounting quirk from a real payload explosion, dave
// needs its OWN neutral count of exactly the bytes it puts on the wire —
// computed with a real BPE tokenizer, not the chars/4 heuristic
// (estimateTokens) that served earlier compaction estimates. This file
// provides that count (countMessageTokens), a TUI investigation command
// (/tokencount), and a better fallback basis for compaction sizing.
//
// OFFLINE GUARANTEE: the tiktoken-go library's default BPE loader
// DOWNLOADS rank files over HTTPS at runtime (cached under $TMPDIR/
// data-gym-cache). That is unacceptable here — a chat bot must never
// depend on reaching openaipublic.blob.core.windows.net to count tokens,
// and a cold cache behind a firewall would silently degrade every count
// to an error. Instead the two rank files we care about are vendored in
// tokendata/ and go:embed'd; init() registers a loader that serves the
// embedded bytes by basename. No network, ever.

import (
	_ "embed" // go:embed directives below (blank import required even for []byte targets)
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

//go:embed tokendata/cl100k_base.tiktoken
var cl100kBaseRanks []byte

//go:embed tokendata/o200k_base.tiktoken
var o200kBaseRanks []byte

// TokenCount is the result of countMessageTokens: dave's own neutral
// count of a message payload, in the units the named encoding produces.
type TokenCount struct {
	// Tokens is the estimated total INCLUDING per-message chat-template
	// overhead and the final-reply priming (see countMessageTokens).
	Tokens int
	// Encoding names the encoding actually used for the count
	// ("cl100k_base", "o200k_base", or "chars/4-estimate" when the
	// tokenizer could not be initialized).
	Encoding string
	// Exact is false whenever the model's REAL tokenizer is unknown and
	// an approximation via o200k_base was used instead. o200k has
	// comparable compression characteristics to modern tokenizers, so
	// the count remains useful for cross-provider comparison — but it is
	// NOT the model's actual tokenizer and must never be labeled as
	// such (notices/logs surface this flag verbatim).
	Exact bool
	// ImageParts counts image parts included at the flat
	// imageTokenEstimate approximation. Surfaced so logs can flag
	// divergence: with N image parts the count carries N×85 tokens of
	// documented lower-bound, and any provider reporting materially more
	// is charging the real resolution-scaled vision cost.
	ImageParts int
	// ToolTokens is the estimated token cost of the tool DEFINITIONS
	// serialized into the request (name + description + parameter
	// schema per tool, plus structural framing). Filled by
	// countRequestTokens; countMessageTokens always leaves it zero.
	ToolTokens int
	// Tools is the number of tool definitions included in ToolTokens
	// (tools with a nil Function are skipped and not counted). Filled by
	// countRequestTokens; countMessageTokens always leaves it zero.
	Tools int
}

// imageTokenEstimate is the flat per-image cost added for image_url
// parts: OpenAI's low-detail vision pricing (85 tokens). A documented
// LOWER BOUND — the real cost scales with resolution when detail is
// auto/high — which is exactly why TokenCount.ImageParts exists: the
// flat number keeps the count reproducible while flagging payloads
// where the provider's number may legitimately exceed ours.
const imageTokenEstimate = 85

// chatTemplateOverheadPerMessage approximates the non-content tokens
// each message costs in the chat wire format
// (<|im_start|>role\n … <|im_end|>\n), and replyPrimingTokens the
// tokens reserved for the assistant's reply at the end of the prompt
// (every message in the payload is followed by one header; the reply
// priming is what tiktoken's own cookbook formula uses for the final
// turn). These are approximations of the OpenAI chat format; other
// providers' templates differ by a few tokens per message — negligible
// next to content size, and constant across turns for ratio purposes.
const (
	chatTemplateOverheadPerMessage = 4
	toolCallOverheadPerCall        = 3
	replyPrimingTokens             = 3
)

// toolDefOverheadTokens approximates the JSON structural overhead per
// serialized tool definition — braces, quotes, commas, the "type":
// "function" wrapper — an approximation in the same spirit as the
// per-message chat-template overhead: constant per tool, negligible
// next to name+description+schema size.
const toolDefOverheadTokens = 7

// embeddedBpeLoader serves tiktoken-go's rank lookups from the
// go:embed'd files in tokendata/. The library passes the full blob URL
// (e.g. "https://openaipublic.blob.core.windows.net/encodings/
// cl100k_base.tiktoken"); only the BASENAME identifies the file, so
// that is all we switch on. Anything else (legacy encodings r50k/p50k/
// gpt2 that dave's models never use) errors cleanly — callers fall back
// rather than download.
type embeddedBpeLoader struct{}

func (embeddedBpeLoader) LoadTiktokenBpe(tiktokenBpeFile string) (map[string]int, error) {
	switch path.Base(tiktokenBpeFile) {
	case "cl100k_base.tiktoken":
		return parseTiktokenRanks(cl100kBaseRanks)
	case "o200k_base.tiktoken":
		return parseTiktokenRanks(o200kBaseRanks)
	default:
		return nil, fmt.Errorf("tokencount: no embedded ranks for %q", tiktokenBpeFile)
	}
}

// parseTiktokenRanks parses the rank-file format: one merge per line,
// "<base64(token)> <rank>", space-separated. Mirrors the library's own
// (unexported) loadTiktokenBpe: blank lines are skipped; a malformed
// line fails the whole load — a truncated or corrupt embedded file must
// fail loudly at first use, not silently produce a broken tokenizer.
func parseTiktokenRanks(data []byte) (map[string]int, error) {
	ranks := make(map[string]int)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) != 2 {
			return nil, fmt.Errorf("tokencount: malformed rank line %q", line)
		}
		token, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, fmt.Errorf("tokencount: bad base64 token in rank line %q: %w", line, err)
		}
		rank, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("tokencount: bad rank in rank line %q: %w", line, err)
		}
		ranks[string(token)] = rank
	}
	return ranks, nil
}

var (
	encodersMu sync.Mutex
	// encoders caches one *tiktoken.Tiktoken per encoding name. A
	// constructed encoder is immutable — safe for concurrent use — so
	// the map (not the encoders) is the only thing the mutex guards.
	// First construction parses ~100k-200k rank lines (a few ms); the
	// cache pays that once per process.
	encoders = make(map[string]*tiktoken.Tiktoken)
)

func init() {
	// MUST run before any encoder is created: tiktoken.GetEncoding
	// consults the package-global loader on first use per encoding, and
	// the default loader would attempt an HTTPS download. Everything in
	// this package funnels through getEncoder, which only ever asks for
	// the two embedded names — the loader cannot be reached for
	// anything else.
	tiktoken.SetBpeLoader(embeddedBpeLoader{})
}

// getEncoder lazily constructs (and caches) the named encoding from the
// embedded rank data. Only "cl100k_base" and "o200k_base" are loadable;
// any other name errors to the caller, who is expected to degrade
// gracefully (countMessageTokens falls back to chars/4).
func getEncoder(name string) (*tiktoken.Tiktoken, error) {
	encodersMu.Lock()
	defer encodersMu.Unlock()
	if t, ok := encoders[name]; ok {
		return t, nil
	}
	t, err := tiktoken.GetEncoding(name)
	if err != nil {
		return nil, err
	}
	encoders[name] = t
	return t, nil
}

// resolveEncodingForModel maps a model name to one of the two embedded
// encodings. Resolution mirrors tiktoken.EncodingForModel's own lookup
// (the exported MODEL_TO_ENCODING / MODEL_PREFIX_TO_ENCODING tables —
// exact match first, then prefix; the prefixes are mutually
// non-overlapping, so map iteration order cannot change the result) but
// resolves only the NAME, keeping encoder construction lazy in
// getEncoder.
//
// Exactness: models whose real encoding is cl100k_base or o200k_base
// (gpt-4o*, gpt-4.1*, gpt-4.5*, gpt-4*, gpt-3.5-turbo*, and — via
// daveExtraModelPrefixes — the o1*/o3*/o4* and chatgpt-* rollouts)
// resolve Exact.
// Everything else — unknown models (grok-*, qwen-*, llama-*), the empty
// model, and legacy text models whose real encoding is one we do not
// embed (r50k/p50k/gpt2) — falls back to o200k_base flagged inexact:
// comparable compression, NOT the model's actual tokenizer.
func resolveEncodingForModel(model string) (string, bool) {
	name, ok := modelEncodingName(model)
	if ok {
		switch name {
		case "cl100k_base", "o200k_base":
			return name, true
		}
	}
	return "o200k_base", false
}

// modelEncodingName is the raw table lookup behind
// tiktoken.EncodingForModel, without constructing an encoder. dave's
// own prefix table (daveExtraModelPrefixes) fills gaps in the library's
// tables: the o-series and chatgpt-* rollouts are documented
// o200k_base models but ship after the vendored library version, and
// labeling them inexact would make the Exact flag lie about otherwise
// correct counts.
func modelEncodingName(model string) (string, bool) {
	if name, ok := tiktoken.MODEL_TO_ENCODING[model]; ok {
		return name, true
	}
	for prefix, name := range tiktoken.MODEL_PREFIX_TO_ENCODING {
		if strings.HasPrefix(model, prefix) {
			return name, true
		}
	}
	for prefix, name := range daveExtraModelPrefixes {
		if strings.HasPrefix(model, prefix) {
			return name, true
		}
	}
	return "", false
}

// daveExtraModelPrefixes documents o200k_base models the vendored
// tiktoken-go tables predate (OpenAI's model-to-encoding docs). Kept
// dave-side instead of forking the lib; prefixes are mutually
// non-overlapping with the lib's.
var daveExtraModelPrefixes = map[string]string{
	"o1":       "o200k_base",
	"o3":       "o200k_base",
	"o4":       "o200k_base",
	"chatgpt-": "o200k_base",
}

// countMessageTokens is THE core counting function: dave's own neutral
// token count of a ChatMessage payload, approximating what a chat wire
// request would cost. Formula per message:
//
//	+4 chat-template overhead (<|im_start|>role\n … <|im_end|>\n)
//	+ tokens(role)
//	+ tokens(m.Content) when non-empty
//	+ Σ tokens(text part) for MultiContent, + imageTokenEstimate per
//	  image part (URL never tokenized — data: URLs are megabytes of
//	  base64 the provider does not count as text), ImageParts++
//	+ tokens(name) + tokens(arguments) + 3 per ToolCall
//	+ tokens(ToolCallID) + 1 for RoleTool rows
//	+ tokens(ReasoningContent) when set
//
// plus replyPrimingTokens (3) once for the final assistant reply. The
// wire format sends MultiContent INSTEAD of Content for user messages,
// so normally only one of the two contributes; counting both when both
// are set is belt-and-suspenders for hand-built messages and can only
// overestimate, never understate, a payload.
//
// EncodeOrdinary is used everywhere — tiktoken.Encode PANICS when text
// matches a disallowed special token (e.g. literal "<|endoftext|" in a
// user message), and a counting helper must never take down the bot.
// Ordinary encoding treats such text as plain text, which is exactly
// what a provider receiving it as content does.
//
// Encoder-init failure (corrupt embedded data — unreachable with the
// vendored files verified at download time) degrades to estimateTokens
// per piece with Encoding "chars/4-estimate", Exact false. The caller
// is never failed.
func countMessageTokens(model string, msgs []ChatMessage) TokenCount {
	encName, exact := resolveEncodingForModel(model)
	enc, err := getEncoder(encName)
	if err != nil {
		exact = false
		encName = "chars/4-estimate"
	}

	// count is the per-piece text counting function: real BPE when the
	// encoder initialized, chars/4 when it did not.
	count := estimateTokens
	if enc != nil {
		count = func(s string) int { return len(enc.EncodeOrdinary(s)) }
	}

	total := replyPrimingTokens
	imageParts := 0
	for _, m := range msgs {
		total += chatTemplateOverheadPerMessage
		total += count(m.Role)
		if m.Content != "" {
			total += count(m.Content)
		}
		for _, p := range m.MultiContent {
			switch p.Type {
			case PartTypeImageURL:
				total += imageTokenEstimate
				imageParts++
			default:
				total += count(p.Text)
			}
		}
		for _, tc := range m.ToolCalls {
			total += count(tc.Function.Name)
			total += count(tc.Function.Arguments)
			total += toolCallOverheadPerCall
		}
		if m.Role == RoleTool && m.ToolCallID != "" {
			total += count(m.ToolCallID)
			total++
		}
		if m.ReasoningContent != "" {
			total += count(m.ReasoningContent)
		}
	}
	return TokenCount{Tokens: total, Encoding: encName, Exact: exact, ImageParts: imageParts}
}

// countToolTokens counts what a request's `tools` array contributes to
// the prompt: per tool, tokens(name) + tokens(description) +
// tokens(JSON-marshal of the parameter schema when non-nil) +
// toolDefOverheadTokens of structural framing. OpenAI-style providers
// fold tool definitions into prompt_tokens, so a neutral count meant to
// be compared against provider numbers must include them (this is the
// bulk of the /tokencount gap where provider prompt far exceeded the
// messages-only count).
//
// Tools with a nil Function are skipped entirely — they are not
// serializable function definitions. A schema that fails to marshal
// counts as 0: a counting helper must never fail, and the real request
// path (jsonschema validation at MCP registration) rejects such tools
// long before here. EncodeOrdinary only, per the rule in
// countMessageTokens. A nil enc degrades to estimateTokens, mirroring
// the message side's encoder-init failure path.
func countToolTokens(enc *tiktoken.Tiktoken, tools []Tool) int {
	count := estimateTokens
	if enc != nil {
		count = func(s string) int { return len(enc.EncodeOrdinary(s)) }
	}
	total := 0
	for _, t := range tools {
		if t.Function == nil {
			continue
		}
		total += toolDefOverheadTokens
		total += count(t.Function.Name)
		total += count(t.Function.Description)
		if t.Function.Parameters != nil {
			if raw, err := json.Marshal(t.Function.Parameters); err == nil {
				total += count(string(raw))
			}
		}
	}
	return total
}

// countRequestTokens is the request-level counterpart of
// countMessageTokens: messages PLUS the tool definitions serialized
// into the same request (Tokens = message tokens + tool tokens, with
// ToolTokens/Tools filled in). This is what a chat request actually
// puts on the wire, and therefore the right comparand for a provider's
// reported prompt_tokens — EXCEPT reasoning replay on Responses API
// chains (session.ResponseID set), where prior turns' reasoning items
// are legitimately re-sent as input and counted by the provider.
// Reasoning replay is real input the model consumes, so it is NOT
// subtracted from the provider side; /tokencount surfaces it separately
// as a labeled upper-bound estimate (sumSessionReasoningTokens) because
// server-side context eviction means the provider may replay less than
// the full prior total.
func countRequestTokens(model string, msgs []ChatMessage, tools []Tool) TokenCount {
	tc := countMessageTokens(model, msgs)
	// Second resolution for the tool schemas: getEncoder is a cached
	// lookup (the message pass above already parsed the rank table), and
	// a nil encoder degrades BOTH sides to estimateTokens consistently,
	// so Encoding/Exact keep describing the whole request.
	encName, _ := resolveEncodingForModel(model)
	enc, _ := getEncoder(encName)
	tc.ToolTokens = countToolTokens(enc, tools)
	for i := range tools {
		if tools[i].Function != nil {
			tc.Tools++
		}
	}
	tc.Tokens += tc.ToolTokens
	return tc
}

// messagesToChat converts DB message rows to their ChatMessage shape
// for counting, mirroring what loadDBSessionMessages + messageFromDB
// would produce (tool calls / multi-content JSON unmarshalled).
func messagesToChat(msgs []Message) []ChatMessage {
	out := make([]ChatMessage, len(msgs))
	for i := range msgs {
		out[i] = messageFromDB(msgs[i])
	}
	return out
}
