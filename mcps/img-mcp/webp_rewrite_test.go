package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assembleTestWebP hand-assembles a RIFF/webp from explicit chunks,
// independently of the production buildWebp, so the container assertions
// below verify framing without leaning on the code under test.
func assembleTestWebP(t *testing.T, chunks []riffChunk) []byte {
	t.Helper()
	var body []byte
	body = append(body, "WEBP"...)
	for _, c := range chunks {
		var sz [4]byte
		binary.LittleEndian.PutUint32(sz[:], uint32(len(c.body)))
		body = append(body, c.fourcc...)
		body = append(body, sz[:]...)
		body = append(body, c.body...)
		if len(c.body)%2 == 1 {
			body = append(body, 0)
		}
	}
	var total [4]byte
	binary.LittleEndian.PutUint32(total[:], uint32(len(body)))
	out := append([]byte("RIFF"), total[:]...)
	return append(out, body...)
}

// parseChunksIndependently re-walks the container with direct binary reads
// (asserting in-bounds sizes and pad bytes as it goes) so the framing checks
// don't share code with the production parser they are verifying.
func parseChunksIndependently(t *testing.T, data []byte) []riffChunk {
	t.Helper()
	require.True(t, isWebpContainer(data), "output must still be a RIFF/WEBP container")
	off := 12
	var out []riffChunk
	for off+8 <= len(data) {
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		require.LessOrEqual(t, off+8+size, len(data), "chunk %q body must be in bounds", string(data[off:off+4]))
		out = append(out, riffChunk{fourcc: string(data[off : off+4]), body: data[off+8 : off+8+size]})
		off += 8 + size
		if size%2 == 1 {
			off++ // spec pad byte after odd-sized bodies
		}
	}
	require.Equal(t, len(data), off, "fixture must end exactly at a chunk boundary")
	return out
}

// TestRewriteNoteInWebpReplacesPayloadAndRoundTrips pins the happy path
// through the file entry point: the note node's text inside the embedded
// workflow is replaced by the enriched payload, and the result parses back
// through the production reader (embeddedWorkflowJSON — imgsite's twin) with
// prompt, provenance, and safety all recoverable. The big int64 seed literal
// must survive verbatim (json.Number fidelity): the metadata has to keep
// describing the generation it accompanied.
func TestRewriteNoteInWebpReplacesPayloadAndRoundTrips(t *testing.T) {
	const enhancedPrompt = "an enhanced majestic cat, studio lighting"
	const bigSeed = int64(9223372036854775807) // 2^63-1: beyond float64's exact range
	workflow := ComfyWorkflow{
		"prompt-node": {Inputs: map[string]interface{}{"text": enhancedPrompt}, Class: "CLIPTextEncode"},
		"output-node": {Inputs: map[string]interface{}{"images": []string{"1"}}, Class: "SaveImage"},
		"seed-node":   {Inputs: map[string]interface{}{"seed": bigSeed}, Class: "RandomNoiseSource"},
		davePromptNoteNodeID: {
			Inputs: map[string]interface{}{"text": `{"prompt":"a cat","llm_generated":false,"job_id":"oldjob"}`},
			Class:  "CLIPTextEncode",
			Meta:   &comfyNodeMeta{Title: davePromptNoteTitle},
		},
	}
	fixture := exifWebPFromWorkflow(t, workflow)

	job := &Job{
		ID:       "newjob",
		Type:     JobTypeGenerate,
		Workflow: "test",
		Input: JobInput{
			Prompt:       "a cat",
			LLMGenerated: true,
			Network:      "graped",
			Channel:      "#test",
			Nick:         "user1",
		},
	}
	noteJSON, err := buildPromptNoteWithSafety(job, "", safetyVerdictUnsafe)
	require.NoError(t, err, "buildPromptNoteWithSafety")

	path := filepath.Join(t.TempDir(), "img_00001_.webp")
	require.NoError(t, os.WriteFile(path, fixture, 0644), "writing fixture")

	require.NoError(t, rewriteNoteInWebp(path, noteJSON), "rewrite must succeed on the file")

	after, err := os.ReadFile(path)
	require.NoError(t, err, "reading rewritten file")

	// Round-trip through the production reader.
	wfJSON, ok := embeddedWorkflowJSON(after)
	require.True(t, ok, "rewritten file must still yield an embedded workflow")
	var wf ComfyWorkflow
	require.NoError(t, json.Unmarshal([]byte(wfJSON), &wf), "rewritten workflow must parse")
	require.Contains(t, wf, davePromptNoteNodeID, "note node must survive the rewrite")
	assert.Equal(t, noteJSON, wf[davePromptNoteNodeID].Inputs["text"],
		"the note node's text must be exactly the enriched payload")
	assert.Equal(t, enhancedPrompt, wf["prompt-node"].Inputs["text"],
		"the enhanced prompt the image ran with must survive untouched")
	assert.Contains(t, wfJSON, "9223372036854775807",
		"int64 seed literals must round-trip exactly (json.Number, not float64)")

	note, ok := embeddedPromptNote(after)
	require.True(t, ok, "note payload must parse back out")
	assert.Equal(t, "a cat", note.Prompt)
	assert.True(t, note.LLMGenerated)
	assert.Equal(t, "newjob", note.JobID)
	assert.Equal(t, "graped", note.Network)
	assert.Equal(t, "#test", note.Channel)
	assert.Equal(t, "user1", note.Nick)
	assert.Equal(t, safetyVerdictUnsafe, note.Safety)
}

// TestRewriteNoteInWebpPreservesOtherChunksAndFixesSizes pins the container
// surgery contract: every chunk except the EXIF is byte-identical (fourcc,
// body, order), the EXIF chunk grows/shrinks to fit the new payload, and the
// RIFF + per-chunk size framing is exactly what the spec says — including the
// pad byte after odd-sized bodies on both sides of the EXIF chunk.
func TestRewriteNoteInWebpPreservesOtherChunksAndFixesSizes(t *testing.T) {
	// The EXIF payload's workflow must carry a note node so the rewrite has
	// something to replace (and so the new EXIF can be parsed back).
	innerWF := ComfyWorkflow{
		"1": {Inputs: map[string]interface{}{"text": "x"}, Class: "X"},
		davePromptNoteNodeID: {
			Inputs: map[string]interface{}{"text": `{"prompt":"a cat","job_id":"old"}`},
			Class:  "CLIPTextEncode",
		},
	}
	innerJSON, err := json.Marshal(innerWF)
	require.NoError(t, err, "marshaling inner workflow")
	tiff := buildTestTIFF("prompt:" + string(innerJSON) + "\x00")
	before := assembleTestWebP(t, []riffChunk{
		{fourcc: "VP8X", body: make([]byte, 10)},
		{fourcc: "ICCP", body: []byte("prof1")}, // odd-sized: pad byte BEFORE the exif chunk
		{fourcc: "EXIF", body: tiff},
		{fourcc: "XMP1", body: []byte("xmp!")}, // chunk AFTER the exif, also preserved
	})

	job := &Job{ID: "sizetest", Input: JobInput{Prompt: "a cat", Network: "graped"}}
	noteJSON, err := buildPromptNoteWithSafety(job, "", safetyVerdictSafe)
	require.NoError(t, err, "buildPromptNoteWithSafety")

	path := filepath.Join(t.TempDir(), "chunks.webp")
	require.NoError(t, os.WriteFile(path, before, 0644), "writing fixture")
	require.NoError(t, rewriteNoteInWebp(path, noteJSON), "rewrite")
	after, err := os.ReadFile(path)
	require.NoError(t, err, "reading rewritten file")

	// RIFF size must describe the whole new file (minus the 8 header bytes).
	riffSize := binary.LittleEndian.Uint32(after[4:8])
	assert.Equal(t, uint32(len(after)-8), riffSize, "RIFF size must match the rewritten file")

	beforeChunks := parseChunksIndependently(t, before)
	afterChunks := parseChunksIndependently(t, after)
	require.Len(t, afterChunks, len(beforeChunks), "chunk count must be unchanged")
	exifSeen := false
	for i := range beforeChunks {
		b, a := beforeChunks[i], afterChunks[i]
		assert.Equal(t, b.fourcc, a.fourcc, "chunk %d fourcc must be unchanged", i)
		if b.fourcc == "EXIF" {
			exifSeen = true
			assert.False(t, bytes.Equal(b.body, a.body), "EXIF body must have been replaced")
			// The new EXIF still parses and carries the new note.
			note, ok := embeddedPromptNote(assembleTestWebP(t, []riffChunk{a}))
			require.True(t, ok, "rewritten EXIF must parse")
			assert.Equal(t, safetyVerdictSafe, note.Safety)
			continue
		}
		assert.True(t, bytes.Equal(b.body, a.body),
			"chunk %q (%s) must be byte-identical after the rewrite", b.fourcc, b.fourcc)
	}
	assert.True(t, exifSeen, "fixture must contain an EXIF chunk")
}

// TestRewriteNoteInWebpRejectsPNG pins the non-webp sentinel: a PNG input
// returns errWebpOnly (the caller WARNs and skips) and the file is left
// untouched.
func TestRewriteNoteInWebpRejectsPNG(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, []byte("IDAT-ish body")...)
	path := filepath.Join(t.TempDir(), "img.png")
	require.NoError(t, os.WriteFile(path, png, 0644), "writing fixture")

	err := rewriteNoteInWebp(path, `{"prompt":"a cat"}`)
	require.ErrorIs(t, err, errWebpOnly, "non-webp input must yield the errWebpOnly sentinel")

	untouched, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.True(t, bytes.Equal(png, untouched), "a rejected rewrite must leave the file untouched")
}

// TestRewriteWebpNoteDataErrorShapes covers the refuse-to-mangle paths: a
// webp with no EXIF chunk, and an embedded workflow with no note node both
// error (caller WARNs and skips) rather than producing a silently degraded
// file.
func TestRewriteWebpNoteDataErrorShapes(t *testing.T) {
	noExif := assembleTestWebP(t, []riffChunk{{fourcc: "VP8X", body: make([]byte, 10)}})
	_, err := rewriteWebpNoteData(noExif, `{"prompt":"a cat"}`)
	require.Error(t, err, "a webp without an EXIF chunk cannot be rewritten")
	assert.Contains(t, err.Error(), "no EXIF chunk")

	noNote := exifWebPWithPrompt(t, "enhanced cat")
	_, err = rewriteWebpNoteData(noNote, `{"prompt":"a cat"}`)
	require.Error(t, err, "a workflow without the note node cannot be rewritten")
	assert.Contains(t, err.Error(), davePromptNoteNodeID)
}

// TestUploadImageToolNoRewriteAndEmptyMeta pins the direct upload_image
// tool's contract: it has no job context, so it sends an empty meta and does
// NOT rewrite the note in the bytes it was handed — there is no verdict or
// provenance to bake, and the caller's bytes are not img-mcp's to alter.
func TestUploadImageToolNoRewriteAndEmptyMeta(t *testing.T) {
	up := newFakeUploadServer(t)
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Upload.URL = up.server.URL
	h := NewToolHandlers(cfg, queuedTestQueue(t))

	fixture := exifWebPWithNote(t, `{"prompt":"a cat","llm_generated":false,"job_id":"direct"}`, "enhanced cat")
	_, out, err := h.handleUploadImage(context.Background(), nil, UploadImageInput{
		Data:     base64.StdEncoding.EncodeToString(fixture),
		Filename: "direct.webp",
	})
	require.NoError(t, err, "handleUploadImage")
	assert.NotEmpty(t, out.URL, "tool must return the upload link")

	assert.Equal(t, "{}", up.gotMetaRaw, "the direct tool has no job context: meta must serialize empty")
	assert.True(t, bytes.Equal(fixture, up.gotFileContent),
		"the direct tool must not rewrite the note in the caller's bytes")
}

// reportableSafety filter check (used by both the note bake and the meta).
func TestReportableSafety(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{safetyVerdictSafe, safetyVerdictSafe},
		{safetyVerdictUnsafe, safetyVerdictUnsafe},
		{safetyVerdictUnknown, ""},       // vetted but unresolved: bake nothing
		{safetyVerdictUnvetted, ""},      // never classified: bake nothing
		{"", ""},                         // same as unvetted
		{"SAFE", ""},                     // case-sensitive: no accidental passes
		{"safe ", ""},                    // whitespace variants are not verdicts
		{"definitely-not-a-verdict", ""}, // hand-edited DB rows degrade, never guess
	} {
		assert.Equal(t, tc.want, reportableSafety(tc.in), "reportableSafety(%q)", tc.in)
	}
}
