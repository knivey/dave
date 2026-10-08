package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Known token vectors, verified against the vendored rank files by
// running the encoders directly (EncodeOrdinary) before pinning:
// "hello world" → 2 tokens in BOTH cl100k_base and o200k_base. If a
// vendored rank file were replaced/truncated these fail loudly — which
// is the point of pinning exact counts rather than ranges.
func TestTokenizerKnownVectors(t *testing.T) {
	cl100k, err := getEncoder("cl100k_base")
	require.NoError(t, err)
	o200k, err := getEncoder("o200k_base")
	require.NoError(t, err)

	assert.Len(t, cl100k.EncodeOrdinary("hello world"), 2)
	assert.Len(t, o200k.EncodeOrdinary("hello world"), 2)

	// A ~200-char English paragraph must yield a plausible count —
	// not a pinned number (word segmentation drifts by a token or two
	// between encodings), just far above the empty/degenerate range.
	paragraph := "The quick brown fox jumps over the lazy dog while twelve eager beavers " +
		"build a dam across the winding river, occasionally pausing to admire the " +
		"reflection of the autumn trees on the still water below the old stone bridge."
	require.Greater(t, len(paragraph), 200)
	assert.Greater(t, len(cl100k.EncodeOrdinary(paragraph)), 30)
	assert.Greater(t, len(o200k.EncodeOrdinary(paragraph)), 30)
}

// TestGetEncoderCachesSingletons: encoder construction parses ~100k+
// rank lines; the second lookup must return the identical pointer.
func TestGetEncoderCachesSingletons(t *testing.T) {
	a, err := getEncoder("cl100k_base")
	require.NoError(t, err)
	b, err := getEncoder("cl100k_base")
	require.NoError(t, err)
	assert.Same(t, a, b)
}

// TestResolveEncodingForModel pins the exactness contract: OpenAI chat
// models resolve to their REAL embedded encoding (Exact=true); unknown
// models (grok-*, qwen-*, llama-*) and the empty model fall back to the
// o200k_base approximation (Exact=false); legacy text models whose real
// encoding is not embedded (gpt2) also fall back rather than error.
func TestResolveEncodingForModel(t *testing.T) {
	cases := []struct {
		model string
		enc   string
		exact bool
	}{
		{"gpt-4o-mini", "o200k_base", true},
		{"gpt-4o", "o200k_base", true},
		{"gpt-4.1", "o200k_base", true},
		{"gpt-4-turbo", "cl100k_base", true},
		{"gpt-4", "cl100k_base", true},
		{"gpt-3.5-turbo", "cl100k_base", true},
		// o-series + chatgpt rollouts: documented o200k_base models the
		// vendored lib tables predate — covered by daveExtraModelPrefixes.
		{"o1", "o200k_base", true},
		{"o1-mini", "o200k_base", true},
		{"o3-mini", "o200k_base", true},
		{"o4-mini", "o200k_base", true},
		{"chatgpt-4o-latest", "o200k_base", true},
		{"grok-4", "o200k_base", false},
		{"qwen3-32b", "o200k_base", false},
		{"llama-3.3-70b", "o200k_base", false},
		{"", "o200k_base", false},
		{"gpt2", "o200k_base", false},
	}
	for _, tc := range cases {
		enc, exact := resolveEncodingForModel(tc.model)
		assert.Equal(t, tc.enc, enc, "model %q", tc.model)
		assert.Equal(t, tc.exact, exact, "model %q", tc.model)
	}
}

// TestCountMessageTokens covers the wire-format approximation formula:
// priming on empty payloads, per-message overhead growth, image parts at
// the flat 85-token lower bound, tool-call argument JSON, tool-role id
// framing, and reasoning content.
func TestCountMessageTokens(t *testing.T) {
	t.Run("empty payload is just priming", func(t *testing.T) {
		tc := countMessageTokens("gpt-4o", nil)
		assert.Equal(t, replyPrimingTokens, tc.Tokens)
		assert.Equal(t, 0, tc.ImageParts)
		assert.Equal(t, "o200k_base", tc.Encoding)
		assert.True(t, tc.Exact)
	})

	t.Run("overhead grows at least four per message", func(t *testing.T) {
		one := countMessageTokens("gpt-4o", []ChatMessage{{Role: RoleUser, Content: ""}})
		two := countMessageTokens("gpt-4o", []ChatMessage{
			{Role: RoleUser, Content: ""},
			{Role: RoleUser, Content: ""},
		})
		assert.GreaterOrEqual(t, two.Tokens-one.Tokens, chatTemplateOverheadPerMessage)
	})

	t.Run("image parts counted at flat estimate", func(t *testing.T) {
		withImage := ChatMessage{Role: RoleUser, MultiContent: []MessagePart{
			{Type: PartTypeText, Text: "what is this"},
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64," + string(make([]byte, 4096))}},
		}}
		textOnly := ChatMessage{Role: RoleUser, MultiContent: []MessagePart{
			{Type: PartTypeText, Text: "what is this"},
		}}
		got := countMessageTokens("gpt-4o", []ChatMessage{withImage})
		assert.Equal(t, 1, got.ImageParts)
		base := countMessageTokens("gpt-4o", []ChatMessage{textOnly})
		assert.Equal(t, 0, base.ImageParts)
		// The multi-kilobyte data URL must NOT be tokenized as text —
		// the image contributes exactly the flat lower bound.
		assert.Equal(t, base.Tokens+imageTokenEstimate, got.Tokens)
	})

	t.Run("tool call arguments are counted", func(t *testing.T) {
		without := []ChatMessage{{Role: RoleAssistant, Content: "checking"}}
		with := []ChatMessage{{
			Role:    RoleAssistant,
			Content: "checking",
			ToolCalls: []ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: FunctionCall{
					Name:      "get_weather",
					Arguments: `{"city":"Tokyo","units":"metric"}`,
				},
			}},
		}}
		got := countMessageTokens("gpt-4o", with)
		base := countMessageTokens("gpt-4o", without)
		// 3 per call plus the name and arguments JSON — strictly more
		// than the bare per-call overhead, so the JSON is demonstrably
		// part of the count.
		assert.Greater(t, got.Tokens, base.Tokens+toolCallOverheadPerCall)
	})

	t.Run("role tool call id adds tokens plus one", func(t *testing.T) {
		const callID = "call_abc123"
		enc, err := getEncoder("o200k_base")
		require.NoError(t, err)
		idTokens := len(enc.EncodeOrdinary(callID))
		require.Greater(t, idTokens, 0)

		without := []ChatMessage{{Role: RoleTool, Content: "result"}}
		with := []ChatMessage{{Role: RoleTool, Content: "result", ToolCallID: callID}}
		got := countMessageTokens("gpt-4o", with)
		base := countMessageTokens("gpt-4o", without)
		assert.Equal(t, base.Tokens+idTokens+1, got.Tokens,
			"tool call id contributes its own tokens plus one framing token")
	})

	t.Run("reasoning content is counted", func(t *testing.T) {
		without := []ChatMessage{{Role: RoleAssistant, Content: "answer"}}
		with := []ChatMessage{{Role: RoleAssistant, Content: "answer", ReasoningContent: "let me think about this carefully"}}
		got := countMessageTokens("gpt-4o", with)
		base := countMessageTokens("gpt-4o", without)
		assert.Greater(t, got.Tokens, base.Tokens)
	})

	t.Run("unknown model is flagged inexact", func(t *testing.T) {
		tc := countMessageTokens("grok-4", []ChatMessage{{Role: RoleUser, Content: "hello"}})
		assert.Equal(t, "o200k_base", tc.Encoding)
		assert.False(t, tc.Exact)
	})

	t.Run("message-only count leaves tool fields zero", func(t *testing.T) {
		// countMessageTokens is the messages-only core: the tool fields
		// belong to countRequestTokens and must stay zero here so the
		// two counters can never be confused for each other.
		tc := countMessageTokens("gpt-4o", []ChatMessage{{Role: RoleUser, Content: "hello"}})
		assert.Equal(t, 0, tc.ToolTokens)
		assert.Equal(t, 0, tc.Tools)
	})

	t.Run("special-token-looking text never panics", func(t *testing.T) {
		// EncodeOrdinary must be used everywhere: tiktoken.Encode PANICS
		// on disallowed special tokens, and user content can contain
		// them verbatim.
		assert.NotPanics(t, func() {
			tc := countMessageTokens("gpt-4o", []ChatMessage{
				{Role: RoleUser, Content: "<|endoftext|><|im_start|>system"},
			})
			assert.Greater(t, tc.Tokens, 0)
		})
	})

	t.Run("concurrent use is race-free", func(t *testing.T) {
		msgs := []ChatMessage{
			{Role: RoleSystem, Content: "you are a bot"},
			{Role: RoleUser, Content: "hello world"},
			{Role: RoleAssistant, Content: "hi there", ReasoningContent: "greeting"},
		}
		want := countMessageTokens("gpt-4o", msgs)
		var wg sync.WaitGroup
		results := make([]TokenCount, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				for j := 0; j < 25; j++ {
					results[slot] = countMessageTokens("gpt-4o", msgs)
				}
			}(i)
		}
		wg.Wait()
		for i := range results {
			assert.Equal(t, want, results[i])
		}
	})
}

// TestCountToolTokens covers the tool-definition counter: empty input,
// nil-Function skipping, the exact per-component formula, and the
// marshal-failure degrade (schema counts as 0, never fails).
func TestCountToolTokens(t *testing.T) {
	enc, err := getEncoder("o200k_base")
	require.NoError(t, err)

	t.Run("empty tool list is zero", func(t *testing.T) {
		assert.Equal(t, 0, countToolTokens(enc, nil))
		assert.Equal(t, 0, countToolTokens(enc, []Tool{}))
	})

	t.Run("nil Function tools are skipped entirely", func(t *testing.T) {
		withNil := []Tool{
			{Type: "function", Function: nil},
			{Type: "function"},
		}
		assert.Equal(t, 0, countToolTokens(enc, withNil))
		// A nil-Function entry between real ones contributes nothing.
		mixed := []Tool{
			{Type: "function", Function: &FunctionDefinition{Name: "aaa", Description: "bbb"}},
			{Type: "function", Function: nil},
		}
		onlyReal := []Tool{
			{Type: "function", Function: &FunctionDefinition{Name: "aaa", Description: "bbb"}},
		}
		assert.Equal(t, countToolTokens(enc, onlyReal), countToolTokens(enc, mixed))
	})

	t.Run("each component counted — name, description, schema, overhead", func(t *testing.T) {
		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string", "description": "the prompt"},
			},
			"required": []string{"prompt"},
		}
		tool := Tool{Type: "function", Function: &FunctionDefinition{
			Name:        "generate_image",
			Description: "Generate an image from a prompt",
			Parameters:  schema,
		}}
		raw, err := json.Marshal(schema)
		require.NoError(t, err)

		nameT := len(enc.EncodeOrdinary("generate_image"))
		descT := len(enc.EncodeOrdinary("Generate an image from a prompt"))
		schemaT := len(enc.EncodeOrdinary(string(raw)))

		got := countToolTokens(enc, []Tool{tool})
		assert.Equal(t, nameT+descT+schemaT+toolDefOverheadTokens, got,
			"formula must be name + description + marshalled schema + overhead")
		// And strictly more than the name-only count, so each component
		// is demonstrably present rather than an equality accident.
		assert.Greater(t, got, nameT+toolDefOverheadTokens)
	})

	t.Run("nil Parameters contribute no schema tokens", func(t *testing.T) {
		withSchema := Tool{Type: "function", Function: &FunctionDefinition{
			Name:        "t",
			Description: "d",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		}}
		without := Tool{Type: "function", Function: &FunctionDefinition{
			Name:        "t",
			Description: "d",
		}}
		assert.Greater(t, countToolTokens(enc, []Tool{withSchema}), countToolTokens(enc, []Tool{without}))
	})

	t.Run("unmarshalable schema counts as zero, never fails", func(t *testing.T) {
		// json.Marshal rejects channels; the counter must degrade to
		// "schema contributes 0" instead of erroring or panicking.
		bad := Tool{Type: "function", Function: &FunctionDefinition{
			Name:        "bad",
			Description: "d",
			Parameters:  make(chan int),
		}}
		equivalentNilParams := Tool{Type: "function", Function: &FunctionDefinition{
			Name:        "bad",
			Description: "d",
		}}
		assert.Equal(t,
			countToolTokens(enc, []Tool{equivalentNilParams}),
			countToolTokens(enc, []Tool{bad}))
	})

	t.Run("nil encoder degrades to estimateTokens", func(t *testing.T) {
		// Single-char strings make the two bases clearly diverge:
		// BPE counts 1 token per char, chars/4 floors each to 0.
		tool := Tool{Type: "function", Function: &FunctionDefinition{Name: "a", Description: "b"}}
		want := toolDefOverheadTokens + estimateTokens("a") + estimateTokens("b")
		assert.Equal(t, want, countToolTokens(nil, []Tool{tool}))
		assert.NotEqual(t, countToolTokens(enc, []Tool{tool}), want,
			"sanity: the encoder count must differ from the chars/4 fallback")
	})
}

// TestCountRequestTokens pins the composition contract: the request
// count is exactly the message count plus the tool count, with the tool
// fields filled in and the message-side fields (ImageParts, Encoding,
// Exact) preserved verbatim.
func TestCountRequestTokens(t *testing.T) {
	enc, err := getEncoder("o200k_base")
	require.NoError(t, err)

	msgs := []ChatMessage{
		{Role: RoleSystem, Content: "you are a bot"},
		{Role: RoleUser, Content: "hello world", MultiContent: []MessagePart{
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64,AAAA"}},
		}},
		{Role: RoleAssistant, Content: "hi", ReasoningContent: "thinking"},
	}
	tools := []Tool{
		{Type: "function", Function: &FunctionDefinition{
			Name:        "generate_image",
			Description: "Generate an image",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		{Type: "function", Function: nil}, // skipped: not counted in Tools
		{Type: "function", Function: &FunctionDefinition{
			Name:        "get_transcript",
			Description: "Fetch a transcript",
		}},
	}

	base := countMessageTokens("gpt-4o", msgs)
	toolSum := countToolTokens(enc, tools)
	req := countRequestTokens("gpt-4o", msgs, tools)

	assert.Equal(t, base.Tokens+toolSum, req.Tokens, "request total = messages + tools")
	assert.Equal(t, toolSum, req.ToolTokens)
	assert.Equal(t, 2, req.Tools, "only tools with non-nil Function count")
	assert.Equal(t, base.ImageParts, req.ImageParts, "message-side fields preserved")
	assert.Equal(t, base.Encoding, req.Encoding)
	assert.Equal(t, base.Exact, req.Exact)

	t.Run("no tools degenerates to the message count", func(t *testing.T) {
		req := countRequestTokens("gpt-4o", msgs, nil)
		base := countMessageTokens("gpt-4o", msgs)
		assert.Equal(t, base.Tokens, req.Tokens)
		assert.Equal(t, 0, req.ToolTokens)
		assert.Equal(t, 0, req.Tools)
	})
}

// TestParseTiktokenRanks exercises the vendored rank-file parser against
// a small in-memory corpus: good lines, blank lines, and each malformed
// shape (bad base64, bad int, missing field) must fail the load.
func TestParseTiktokenRanks(t *testing.T) {
	t.Run("good lines", func(t *testing.T) {
		// "hi" → aGk=, "hello" → aGVsbG8=
		ranks, err := parseTiktokenRanks([]byte("aGk= 5\naGVsbG8= 10\n"))
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"hi": 5, "hello": 10}, ranks)
	})

	t.Run("blank lines skipped", func(t *testing.T) {
		ranks, err := parseTiktokenRanks([]byte("aGk= 1\n\naGVsbG8= 2\n"))
		require.NoError(t, err)
		assert.Len(t, ranks, 2)
	})

	t.Run("bad base64 fails", func(t *testing.T) {
		_, err := parseTiktokenRanks([]byte("!!!not-base64!!! 3\n"))
		require.Error(t, err)
	})

	t.Run("bad rank int fails", func(t *testing.T) {
		_, err := parseTiktokenRanks([]byte("aGk= notanumber\n"))
		require.Error(t, err)
	})

	t.Run("missing rank field fails", func(t *testing.T) {
		_, err := parseTiktokenRanks([]byte("aGk=\n"))
		require.Error(t, err)
	})
}

// TestEmbeddedBpeLoaderServesByBasename: the loader receives full blob
// URLs from tiktoken-go and must map them to the embedded files; the
// embedded parses must be full-size rank tables (a truncated vendored
// file would produce far fewer entries); anything not vendored errors
// instead of reaching for the network.
func TestEmbeddedBpeLoaderServesByBasename(t *testing.T) {
	const blobBase = "https://openaipublic.blob.core.windows.net/encodings/"

	ranks, err := embeddedBpeLoader{}.LoadTiktokenBpe(blobBase + "cl100k_base.tiktoken")
	require.NoError(t, err)
	assert.Greater(t, len(ranks), 100000, "cl100k_base rank table must be full-size")

	ranks, err = embeddedBpeLoader{}.LoadTiktokenBpe(blobBase + "o200k_base.tiktoken")
	require.NoError(t, err)
	assert.Greater(t, len(ranks), 190000, "o200k_base rank table must be full-size")

	_, err = embeddedBpeLoader{}.LoadTiktokenBpe(blobBase + "r50k_base.tiktoken")
	assert.Error(t, err, "non-vendored encodings must error, never download")
}

// TestCompactSessionLiveTokensEstTokenizerDerived pins the Phase E
// switch end to end: with a model configured, LiveTokensEst is
// countMessageTokens over the post-compaction live rows (real tokenizer
// accounting, including per-message overhead), NOT the chars/4 sum over
// Content. The seed content — many short words — makes the two counts
// clearly divergent (chars/4 underestimates word-heavy text), so the
// switch cannot silently regress to the old basis. The no-model path
// keeps the estimateTokens fallback.
func TestCompactSessionLiveTokensEstTokenizerDerived(t *testing.T) {
	// "a b c … t": 39 runes → estimateTokens 9, but 20 one-char words +
	// spaces ≈ 20+ tokenizer tokens — plus 4/message overhead the old
	// sum never counted.
	const shortWords = "a b c d e f g h i j k l m n o p q r s t"

	setupTestDB(t)

	stub, getBodies := newRecordingSummarizerStubServer(t, "SUMMARY_TEXT")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: shortWords}))
	for i := 0; i < 6; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: shortWords}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: shortWords}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	res, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)
	require.Len(t, getBodies(), 1)

	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Equal(t, res.LiveMessages, len(live))

	charsSum := 0
	for _, m := range live {
		charsSum += estimateTokens(m.Content)
	}
	want := countMessageTokens("m", messagesToChat(live)).Tokens
	require.NotEqual(t, want, charsSum,
		"seed sanity: tokenizer count and chars/4 sum must diverge for the assertion to mean anything")
	assert.Equal(t, want, res.LiveTokensEst,
		"LiveTokensEst must be tokenizer-derived when a model is configured")
}

// TestCompactSessionLiveTokensEstNoModelFallback pins the fallback half
// of the Phase E switch: with NO model configured, LiveTokensEst stays
// the chars/4 sum over the live rows' Content (tokensPerMessage's floor
// path has the same never-fail requirement).
func TestCompactSessionLiveTokensEstNoModelFallback(t *testing.T) {
	setupTestDB(t)

	stub, _ := newRecordingSummarizerStubServer(t, "SUMMARY_TEXT")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 6; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "", Timeout: 5 * time.Second, MaxTokens: 256}
	res, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	charsSum := 0
	for _, m := range live {
		charsSum += estimateTokens(m.Content)
	}
	assert.Equal(t, charsSum, res.LiveTokensEst,
		"LiveTokensEst must fall back to the chars/4 sum when no model is configured")
}

// TestMessagesToChat verifies the DB-row → ChatMessage conversion used
// by the counting paths preserves the fields the counter weighs
// (tool-call JSON, multi-content, reasoning, tool ids).
func TestMessagesToChat(t *testing.T) {
	toolCalls := `[{"index":0,"id":"call_9","type":"function","function":{"name":"f","arguments":"{}"}}]`
	multi := `[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:x"}}]`
	reasoning := "because"
	toolID := "call_9"
	msgs := []Message{
		{Role: RoleAssistant, Content: "x", ToolCalls: &toolCalls, ReasoningContent: &reasoning},
		{Role: RoleUser, Content: "", MultiContent: &multi},
		{Role: RoleTool, Content: "res", ToolCallID: &toolID},
	}
	out := messagesToChat(msgs)
	require.Len(t, out, 3)
	require.Len(t, out[0].ToolCalls, 1)
	assert.Equal(t, "f", out[0].ToolCalls[0].Function.Name)
	assert.Equal(t, reasoning, out[0].ReasoningContent)
	require.Len(t, out[1].MultiContent, 2)
	assert.Equal(t, "hi", out[1].MultiContent[0].Text)
	assert.Equal(t, toolID, out[2].ToolCallID)

	// Determinism for the counter: same rows → same count.
	a := countMessageTokens("gpt-4o", messagesToChat(msgs))
	b := countMessageTokens("gpt-4o", messagesToChat(msgs))
	assert.Equal(t, a, b)
}
