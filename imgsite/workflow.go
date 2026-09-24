package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// davePromptNoteNodeID is the fixed node ID img-mcp injects for the
// original-prompt note (a disconnected CLIPTextEncode whose text is a JSON
// payload). It is the ONE place extraction keys on a node ID instead of
// class_type — the ID is the wire contract between img-mcp and imgsite.
// Everything else keys on class_type + edge references because node IDs
// are NOT stable (production graphs use 55-76 where the checked-in
// workflow template uses 3-29 for the same logical graph).
const davePromptNoteNodeID = "dave_original_prompt"

// workflowNode is one node of a ComfyUI API-format graph:
// {"<id>": {"class_type": "...", "inputs": {...}}}.
//
// Numbers decode as json.Number (UseNumber) because seeds are full int64s
// (e.g. 5702895218061442231) that float64's 53-bit mantissa would round.
type workflowNode struct {
	ClassType string         `json:"class_type"`
	Inputs    map[string]any `json:"inputs"`
}

// promptNotePayload is the JSON carried by the note node's text input;
// mirrors img-mcp's buildPromptNote contract.
type promptNotePayload struct {
	Prompt               string `json:"prompt"`
	LLMGenerated         bool   `json:"llm_generated"`
	JobID                string `json:"job_id"`
	EnhancementReasoning string `json:"enhancement_reasoning,omitempty"`
}

// extractedMetadata is everything the images table stores that can be
// derived from the embedded workflow graph. Zero values mean "not found" —
// extraction degrades per-field, never fails wholesale.
type extractedMetadata struct {
	OriginalPrompt string
	EnhancedPrompt string
	NegativePrompt string
	Reasoning      string
	JobID          string
	LLMGenerated   bool

	Seed      *int64
	Steps     *int
	Cfg       *float64
	Denoise   *float64
	Sampler   string
	Scheduler string

	ModelUnet string
	ModelClip string
	ModelVae  string
	LorasJSON string // JSON array [{"name":…,"strength":…}]

	Width  *int
	Height *int

	WorkflowJSON string // pretty-printed full graph (numbers verbatim)
}

// loraEntry serializes into the loras column's JSON array.
type loraEntry struct {
	Name     string   `json:"name"`
	Strength *float64 `json:"strength,omitempty"`
}

// parsePromptPayload decodes the note node's JSON text. Failure yields
// ok=false and the caller leaves the note-derived fields blank.
func parsePromptPayload(s string) (promptNotePayload, bool) {
	var pn promptNotePayload
	if err := json.Unmarshal([]byte(s), &pn); err != nil {
		return pn, false
	}
	return pn, true
}

// parseWorkflowGraph decodes an API-format workflow JSON with
// json.Number fidelity (see workflowNode).
func parseWorkflowGraph(payload string) (map[string]workflowNode, bool) {
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	var wf map[string]workflowNode
	if err := dec.Decode(&wf); err != nil || wf == nil {
		return nil, false
	}
	return wf, true
}

// ExtractMetadata parses an API-format workflow payload and derives every
// graph-derived field. ok=false only when the payload is not a decodable
// graph object; individual field failures degrade to blanks.
func ExtractMetadata(payload string) (extractedMetadata, bool) {
	wf, ok := parseWorkflowGraph(payload)
	if !ok {
		return extractedMetadata{}, false
	}
	md := extractFromGraph(wf)
	md.WorkflowJSON = prettyJSON(payload)
	return md, true
}

// prettyJSON re-indents the ORIGINAL payload with json.Indent, which
// copies tokens lexically — big seed integers stay byte-identical, unlike
// a decode/re-marshal round trip through float64.
func prettyJSON(payload string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(payload), "", "  "); err != nil {
		return payload
	}
	return buf.String()
}

// extractFromGraph applies the class_type-keyed rules. Node iteration is
// always in sorted-ID order so results never depend on Go's randomized
// map iteration.
func extractFromGraph(wf map[string]workflowNode) extractedMetadata {
	var md extractedMetadata
	ids := make([]string, 0, len(wf))
	for id := range wf {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Note node: fixed-ID contract, JSON payload, failures degrade.
	if node, ok := wf[davePromptNoteNodeID]; ok {
		if text, ok := strInput(node, "text"); ok {
			if pn, ok := parsePromptPayload(text); ok {
				md.OriginalPrompt = pn.Prompt
				md.LLMGenerated = pn.LLMGenerated
				md.JobID = pn.JobID
				md.Reasoning = pn.EnhancementReasoning
			}
		}
	}

	// Sampler: params plus positive/negative conditioning endpoints.
	if sampler, ok := pickSampler(wf, ids); ok {
		md.Seed = nodeInt64(sampler, "seed")
		md.Steps = nodeInt(sampler, "steps")
		md.Cfg = nodeFloat(sampler, "cfg")
		md.Denoise = nodeFloat(sampler, "denoise")
		md.Sampler, _ = strInput(sampler, "sampler_name")
		md.Scheduler, _ = strInput(sampler, "scheduler")
		md.EnhancedPrompt = conditioningText(wf, sampler.Inputs["positive"])
		md.NegativePrompt = conditioningText(wf, sampler.Inputs["negative"])
	}

	var loras []loraEntry
	for _, id := range ids {
		node := wf[id]
		ct := node.ClassType
		switch {
		case ct == "UNETLoader" && md.ModelUnet == "":
			md.ModelUnet, _ = strInput(node, "unet_name")
		case ct == "CLIPLoader" && md.ModelClip == "":
			md.ModelClip, _ = strInput(node, "clip_name")
		case ct == "VAELoader" && md.ModelVae == "":
			md.ModelVae, _ = strInput(node, "vae_name")
		}
		if strings.HasPrefix(ct, "LoraLoader") {
			if name, ok := strInput(node, "lora_name"); ok {
				entry := loraEntry{Name: name}
				// LoraLoaderModelOnly carries strength_model; full
				// LoraLoader also has strength_clip — prefer the model
				// strength.
				if s := nodeFloat(node, "strength_model"); s != nil {
					entry.Strength = s
				} else if s := nodeFloat(node, "strength_clip"); s != nil {
					entry.Strength = s
				}
				loras = append(loras, entry)
			}
		}
		// EmptyLatentImage / EmptySD3LatentImage (and future Empty* variants).
		// Dims are atomic: only recorded when BOTH width and height are
		// present, so a partial node can't plant 1920×0.
		if strings.HasPrefix(ct, "Empty") && strings.HasSuffix(ct, "LatentImage") && md.Width == nil {
			if w := nodeInt(node, "width"); w != nil {
				if h := nodeInt(node, "height"); h != nil {
					md.Width, md.Height = w, h
				}
			}
		}
	}
	if len(loras) > 0 {
		if b, err := json.Marshal(loras); err == nil {
			md.LorasJSON = string(b)
		}
	}

	return md
}

// pickSampler chooses among KSampler/KSamplerAdvanced nodes deterministi-
// cally: the first (in sorted-ID order) with a complete seed/steps/cfg
// set, else the first sampler at all. Production graphs have exactly one;
// the rule only matters for hand-built multi-sampler graphs.
func pickSampler(wf map[string]workflowNode, ids []string) (workflowNode, bool) {
	var first workflowNode
	found := false
	for _, id := range ids {
		node, ok := wf[id]
		if !ok {
			continue
		}
		if node.ClassType != "KSampler" && node.ClassType != "KSamplerAdvanced" {
			continue
		}
		if !found {
			first, found = node, true
		}
		if nodeInt64(node, "seed") != nil && nodeInt(node, "steps") != nil && nodeFloat(node, "cfg") != nil {
			return node, true
		}
	}
	return first, found
}

// conditioningText follows a sampler's positive/negative edge to its text.
// ComfyUI refs are [nodeID, slot] tuples. Endpoint rules per the verified
// production format: ConditioningZeroOut/ConditioningCombine carry no
// text (zeroed/combined conditioning); CLIPTextEncode's text is the
// prompt. IMPORTANT: an unreferenced CLIPTextEncode (production graphs
// keep an orphan negative-prompt node that the sampler does NOT use) must
// never be picked up — which is exactly why we only ever follow the
// sampler's edge instead of scanning text nodes.
func conditioningText(wf map[string]workflowNode, v any) string {
	ref, ok := refNodeID(v)
	if !ok {
		return ""
	}
	node, ok := wf[ref]
	if !ok {
		return ""
	}
	switch node.ClassType {
	case "ConditioningZeroOut", "ConditioningCombine":
		return ""
	case "CLIPTextEncode":
		text, _ := strInput(node, "text")
		return text
	}
	return ""
}

// refNodeID extracts the target node ID from a [nodeID, slot] ref value.
func refNodeID(v any) (string, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return "", false
	}
	id, ok := arr[0].(string)
	return id, ok
}

func strInput(node workflowNode, key string) (string, bool) {
	v, ok := node.Inputs[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// nodeInt64 / nodeInt / nodeFloat read numeric inputs. json.Number (the
// UseNumber decode path) preserves int64 seeds exactly; float64 is
// accepted too for callers that built maps by hand.
func nodeInt64(node workflowNode, key string) *int64 {
	v, ok := node.Inputs[key]
	if !ok {
		return nil
	}
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return &i
		}
		if f, err := n.Float64(); err == nil {
			i := int64(f)
			return &i
		}
	case float64:
		i := int64(n)
		return &i
	}
	return nil
}

func nodeInt(node workflowNode, key string) *int {
	if i := nodeInt64(node, key); i != nil {
		v := int(*i)
		return &v
	}
	return nil
}

func nodeFloat(node workflowNode, key string) *float64 {
	v, ok := node.Inputs[key]
	if !ok {
		return nil
	}
	switch n := v.(type) {
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return &f
		}
	case float64:
		return &n
	}
	return nil
}
