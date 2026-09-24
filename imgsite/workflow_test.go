package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const zImageTurboPath = "../mcps/img-mcp/workflows/z_image_turbo.json"

// Expected extraction from the checked-in workflow template (IDs 3-29,
// includes LoraLoaderModelOnly and a real CLIPTextEncode negative).
var zImageTurboWant = extractedMetadata{
	OriginalPrompt: "", // template file carries no injected note node
	EnhancedPrompt: "cute anime style girl with massive fluffy fennec ears and a big fluffy tail blonde messy long hair blue eyes wearing a maid outfit with a long black gold leaf pattern dress and a white apron, it is a postcard held by a hand in front of a beautiful realistic city at sunset and there is cursive writing that says \"ZImage, Now in ComfyUI\"",
	NegativePrompt: "blurry ugly bad",
	Seed:           int64Ptr(799008282954756),
	Steps:          intPtr(9),
	Cfg:            float64Ptr(1),
	Denoise:        float64Ptr(1),
	Sampler:        "euler",
	Scheduler:      "simple",
	ModelUnet:      "z_image_turbo_bf16.safetensors",
	ModelClip:      "qwen_3_4b_fp8_mixed.safetensors",
	ModelVae:       "zimage-ae.safetensors",
	LorasJSON:      `[{"name":"ZIT/PepeZimage.safetensors","strength":0.2}]`,
	Width:          intPtr(1600),
	Height:         intPtr(900),
}

func intPtr(v int) *int             { return &v }
func int64Ptr(v int64) *int64       { return &v }
func float64Ptr(v float64) *float64 { return &v }

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// renumberGraph rewrites every node ID (and every [nodeID, slot] edge ref)
// through the mapping, re-marshaling the graph. This is the ID-agnosticism
// proof: same logical graph, completely different numbering.
func renumberGraph(t *testing.T, payload string, mapping map[string]string) string {
	t.Helper()
	var wf map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &wf))
	out := make(map[string]any, len(wf))
	for id, nodeAny := range wf {
		node, ok := nodeAny.(map[string]any)
		require.True(t, ok, "node %s is not an object", id)
		if inputs, ok := node["inputs"].(map[string]any); ok {
			for _, v := range inputs {
				ref, ok := v.([]any)
				if !ok || len(ref) == 0 {
					continue
				}
				if refID, ok := ref[0].(string); ok {
					if newID, ok := mapping[refID]; ok {
						ref[0] = newID
					}
				}
			}
		}
		newID := id
		if m, ok := mapping[id]; ok {
			newID = m
		}
		out[newID] = node
	}
	b, err := json.Marshal(out)
	require.NoError(t, err)
	return string(b)
}

func assertSameExtraction(t *testing.T, want, got extractedMetadata) {
	t.Helper()
	// WorkflowJSON differs by construction (different raw payloads);
	// everything semantic must match.
	wantJSON := got.WorkflowJSON
	want.WorkflowJSON, got.WorkflowJSON = "", ""
	assert.Equal(t, want, got)
	assert.NotEmpty(t, wantJSON)
}

func TestExtractMetadataZImageTurbo(t *testing.T) {
	payload := mustReadFile(t, zImageTurboPath)

	md, ok := ExtractMetadata(payload)

	require.True(t, ok)
	assertSameExtraction(t, zImageTurboWant, md)
	// The raw graph survives verbatim in the pretty-printed JSON.
	assert.Contains(t, md.WorkflowJSON, "799008282954756")
	assert.Contains(t, md.WorkflowJSON, "LoraLoaderModelOnly")
}

// TestExtractMetadataIDAgnostic renumbers the checked-in graph onto the
// production numbering style (55-76) and demands identical extraction —
// the core "key on class_type, never node IDs" guarantee.
func TestExtractMetadataIDAgnostic(t *testing.T) {
	payload := mustReadFile(t, zImageTurboPath)
	mapping := map[string]string{
		"3": "58", "6": "56", "7": "76", "8": "59", "13": "57",
		"16": "60", "17": "62", "18": "61", "28": "55", "29": "64",
	}
	renumbered := renumberGraph(t, payload, mapping)

	md, ok := ExtractMetadata(renumbered)

	require.True(t, ok)
	assertSameExtraction(t, zImageTurboWant, md)
}

// syntheticGraph builds a production-shaped graph: sampler with note node,
// configurable negative endpoint.
func syntheticGraph(negativeClass string, noteText string) string {
	graph := map[string]any{
		"56": map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "enhanced text"}},
		"57": map[string]any{"class_type": "EmptySD3LatentImage", "inputs": map[string]any{"width": 100, "height": 50}},
		"58": map[string]any{"class_type": "KSampler", "inputs": map[string]any{
			"seed": 42, "steps": 6, "cfg": 2.5, "denoise": 0.75,
			"sampler_name": "dpmpp_2m", "scheduler": "karras",
			"positive": []any{"56", 0}, "negative": []any{"63", 0},
		}},
		"60": map[string]any{"class_type": "UNETLoader", "inputs": map[string]any{"unet_name": "u.safetensors"}},
		"62": map[string]any{"class_type": "VAELoader", "inputs": map[string]any{"vae_name": "v.safetensors"}},
	}
	if negativeClass == "ConditioningZeroOut" {
		graph["63"] = map[string]any{"class_type": "ConditioningZeroOut", "inputs": map[string]any{"conditioning": []any{"56", 0}}}
	} else {
		graph["63"] = map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "neg words"}}
		graph["76"] = map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "orphan negative never used"}}
	}
	if noteText != "" {
		graph["dave_original_prompt"] = map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": noteText}}
	}
	b, _ := json.Marshal(graph)
	return string(b)
}

func TestExtractMetadataNegativeEndpoints(t *testing.T) {
	t.Run("ConditioningZeroOutIsEmpty", func(t *testing.T) {
		md, ok := ExtractMetadata(syntheticGraph("ConditioningZeroOut", ""))
		require.True(t, ok)
		assert.Empty(t, md.NegativePrompt, "zeroed conditioning carries no negative text")
		assert.Equal(t, "enhanced text", md.EnhancedPrompt)
	})
	t.Run("CLIPTextEncodeNegativeFollowed", func(t *testing.T) {
		md, ok := ExtractMetadata(syntheticGraph("CLIPTextEncode", ""))
		require.True(t, ok)
		assert.Equal(t, "neg words", md.NegativePrompt, "the negative endpoint's text")
		assert.NotEqual(t, "orphan negative never used", md.NegativePrompt, "an unreferenced text node must never win")
		assert.Equal(t, "enhanced text", md.EnhancedPrompt)
	})
}

func TestExtractMetadataNotePayload(t *testing.T) {
	note := `{"prompt":"a cat","llm_generated":true,"job_id":"j1","enhancement_reasoning":"because"}`
	md, ok := ExtractMetadata(syntheticGraph("ConditioningZeroOut", note))
	require.True(t, ok)
	assert.Equal(t, "a cat", md.OriginalPrompt)
	assert.True(t, md.LLMGenerated)
	assert.Equal(t, "j1", md.JobID)
	assert.Equal(t, "because", md.Reasoning)
}

func TestExtractMetadataNoteParseFailureDegrades(t *testing.T) {
	md, ok := ExtractMetadata(syntheticGraph("ConditioningZeroOut", "this is not json"))

	require.True(t, ok, "graph still extracts when the note payload is garbage")
	assert.Empty(t, md.OriginalPrompt)
	assert.Empty(t, md.JobID)
	assert.Empty(t, md.Reasoning)
	assert.False(t, md.LLMGenerated)
	assert.Equal(t, "enhanced text", md.EnhancedPrompt, "sampler-derived fields unaffected")
	require.NotNil(t, md.Seed)
	assert.EqualValues(t, 42, *md.Seed)
}

func TestExtractMetadataInvalidPayload(t *testing.T) {
	_, ok := ExtractMetadata("not json at all")
	assert.False(t, ok)

	_, ok = ExtractMetadata(`[1,2,3]`)
	assert.False(t, ok, "a JSON array is not a workflow graph")
}

func TestExtractMetadataLatentDimsAtomic(t *testing.T) {
	// A latent node with only width must not plant half a dimension pair.
	payload := `{"57":{"class_type":"EmptyLatentImage","inputs":{"width":1920}}}`
	md, ok := ExtractMetadata(payload)
	require.True(t, ok)
	assert.Nil(t, md.Width, "partial dims are dropped, not stored as 1920×0")
	assert.Nil(t, md.Height)

	payload = `{"57":{"class_type":"EmptyLatentImage","inputs":{"width":1920,"height":1080}}}`
	md, ok = ExtractMetadata(payload)
	require.True(t, ok)
	require.NotNil(t, md.Width)
	assert.Equal(t, 1920, *md.Width)
	require.NotNil(t, md.Height)
	assert.Equal(t, 1080, *md.Height)
}

func TestExtractMetadataMissingSampler(t *testing.T) {
	md, ok := ExtractMetadata(`{"57":{"class_type":"EmptyLatentImage","inputs":{"width":8,"height":8}}}`)
	require.True(t, ok)
	assert.Nil(t, md.Seed)
	assert.Empty(t, md.EnhancedPrompt)
	require.NotNil(t, md.Width)
	assert.Equal(t, 8, *md.Width)
}

// TestExtractMetadataDeterministicAcrossMapOrder extracts the same graph
// repeatedly — Go map iteration order is randomized, results must not be.
func TestExtractMetadataDeterministicAcrossMapOrder(t *testing.T) {
	payload := mustReadFile(t, zImageTurboPath)
	first, ok := ExtractMetadata(payload)
	require.True(t, ok)
	for i := 0; i < 20; i++ {
		md, ok := ExtractMetadata(payload)
		require.True(t, ok)
		assertSameExtraction(t, first, md)
	}
}

// TestParsePromptPayload covers the note JSON contract directly.
func TestParsePromptPayload(t *testing.T) {
	t.Run("Full", func(t *testing.T) {
		pn, ok := parsePromptPayload(`{"prompt":"p","llm_generated":true,"job_id":"j","enhancement_reasoning":"r"}`)
		require.True(t, ok)
		assert.Equal(t, "p", pn.Prompt)
		assert.True(t, pn.LLMGenerated)
		assert.Equal(t, "j", pn.JobID)
		assert.Equal(t, "r", pn.EnhancementReasoning)
	})
	t.Run("OmitsOptionals", func(t *testing.T) {
		pn, ok := parsePromptPayload(`{"prompt":"p","llm_generated":false,"job_id":"j"}`)
		require.True(t, ok)
		assert.Empty(t, pn.EnhancementReasoning)
	})
	t.Run("UnknownFieldsIgnored", func(t *testing.T) {
		_, ok := parsePromptPayload(`{"prompt":"p","future":"x"}`)
		assert.True(t, ok)
	})
	t.Run("Garbage", func(t *testing.T) {
		_, ok := parsePromptPayload("nope")
		assert.False(t, ok)
	})
}
