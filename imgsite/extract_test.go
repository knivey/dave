package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── Synthetic container/TIFF builders (adversarial fixtures) ─────────────

// buildTIFF encodes a one-entry IFD0 (Model tag, ASCII) TIFF in either
// byte order, mirroring the verified production structure.
func buildTIFF(payload string, littleEndian bool) []byte {
	bo := binary.ByteOrder(binary.BigEndian)
	mark := []byte("MM")
	if littleEndian {
		bo = binary.LittleEndian
		mark = []byte("II")
	}
	header := make([]byte, 8)
	copy(header[0:2], mark)
	bo.PutUint16(header[2:4], 42)
	bo.PutUint32(header[4:8], 8) // IFD0 immediately follows the header

	entry := make([]byte, 12)
	bo.PutUint16(entry[0:2], exifTagModel)
	bo.PutUint16(entry[2:4], tiffTypeASCII)
	bo.PutUint32(entry[4:8], uint32(len(payload)))
	bo.PutUint32(entry[8:12], 8+2+12+4) // value follows header+count+entry+next

	ifd := make([]byte, 18)
	bo.PutUint16(ifd[0:2], 1) // one entry
	copy(ifd[2:14], entry)
	// next-IFD offset stays 0

	out := append(header, ifd...)
	out = append(out, payload...)
	return out
}

// buildWebP assembles a minimal RIFF/webp (VP8X + optional EXIF chunk),
// applying the odd-size pad byte per the container spec.
func buildWebP(exif []byte, includeEXIF bool) []byte {
	body := []byte("WEBP")
	body = append(body, []byte("VP8X")...)
	body = append(body, 10, 0, 0, 0)
	body = append(body, make([]byte, 10)...)
	if includeEXIF {
		var sz [4]byte
		binary.LittleEndian.PutUint32(sz[:], uint32(len(exif)))
		body = append(body, []byte("EXIF")...)
		body = append(body, sz[:]...)
		body = append(body, exif...)
		if len(exif)%2 == 1 {
			body = append(body, 0)
		}
	}
	var total [4]byte
	binary.LittleEndian.PutUint32(total[:], uint32(len(body)))
	out := append([]byte("RIFF"), total[:]...)
	return append(out, body...)
}

// buildPNG assembles a minimal PNG with one tEXt chunk (CRC bytes are
// zeroed; the scanner skips them).
func buildPNG(key, value string) []byte {
	data := append([]byte(key), 0)
	data = append(data, value...)
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	var lenBE [4]byte
	binary.BigEndian.PutUint32(lenBE[:], uint32(len(data)))
	buf.Write(lenBE[:])
	buf.Write([]byte("tEXt"))
	buf.Write(data)
	buf.Write([]byte{0, 0, 0, 0}) // fake CRC
	binary.BigEndian.PutUint32(lenBE[:], 0)
	buf.Write(lenBE[:])
	buf.Write([]byte("IEND"))
	buf.Write([]byte{0, 0, 0, 0})
	return buf.Bytes()
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err, "reading testdata/%s", name)
	return data
}

// goldenGraph is the minimal graph both fixture files share (node IDs and
// values taken from the verified production structure).
const fixtureEnhancedPlain = "Dynamic action scene of a small fierce shrew mammal charging straight toward the viewer at high speed, 'comin in hot', dust and debris kicking up around its tiny body, motion blur on its legs, expressive determined eyes, highly detailed fur texture, dramatic cinematic lighting with strong highlights and shadows, vibrant illustrative style, sharp focus, high detail, 8k resolution, epic composition"
const fixtureEnhanced528 = "A cute anthropomorphic brown shrew with detailed fur walking upright down the main street of a charming small American town on a sunny afternoon, wearing a tiny vest and hat, realistic style with vibrant colors, old brick buildings and storefronts lining the street, people in the distance, blue sky with fluffy clouds, photorealistic details, cinematic lighting, high resolution, sharp focus, 8k quality"

// ─── Golden tests against the verified production fixtures ────────────────

func TestExtractGoldenPlainWebP(t *testing.T) {
	data := loadFixture(t, "plain.webp")

	api, ui, found := extractEmbeddedWorkflows(data)

	require.True(t, found, "plain.webp must expose an API workflow payload")
	assert.Empty(t, ui, "plain.webp carries no UI graph")
	assert.Contains(t, api, "dave_original_prompt")

	md, ok := ExtractMetadata(api)
	require.True(t, ok)

	assert.Equal(t, "shrew comin in hot", md.OriginalPrompt)
	assert.False(t, md.LLMGenerated)
	assert.Equal(t, "4eef6f8b", md.JobID)
	assert.Empty(t, md.Reasoning, "plain generation carries no enhancement_reasoning")
	assert.Equal(t, fixtureEnhancedPlain, md.EnhancedPrompt)
	assert.Empty(t, md.NegativePrompt, "sampler negative feeds ConditioningZeroOut")
	require.NotNil(t, md.Seed)
	assert.EqualValues(t, 9034043319117902157, *md.Seed, "19-digit seed must survive exactly (json.Number path)")
	require.NotNil(t, md.Steps)
	assert.Equal(t, 8, *md.Steps)
	require.NotNil(t, md.Cfg)
	assert.InDelta(t, 1.0, *md.Cfg, 0.0001)
	require.NotNil(t, md.Denoise)
	assert.InDelta(t, 1.0, *md.Denoise, 0.0001)
	assert.Equal(t, "euler", md.Sampler)
	assert.Equal(t, "simple", md.Scheduler)
	assert.Equal(t, "krea2_turbo_int8_convrot.safetensors", md.ModelUnet)
	assert.Equal(t, "qwen3vl_4b_fp8_scaled.safetensors", md.ModelClip)
	assert.Equal(t, "qwen_image_vae.safetensors", md.ModelVae)
	assert.Empty(t, md.LorasJSON, "production graphs carry no LoRAs")
	require.NotNil(t, md.Width)
	assert.Equal(t, 1920, *md.Width)
	require.NotNil(t, md.Height)
	assert.Equal(t, 1080, *md.Height)

	// Workflow JSON is pretty-printed with numbers verbatim — the big
	// seed must appear byte-identical (float64 re-marshal would corrupt it).
	assert.Contains(t, md.WorkflowJSON, "\n  \"")
	assert.Contains(t, md.WorkflowJSON, "9034043319117902157")
	assert.NotContains(t, md.WorkflowJSON, "e+", "no scientific notation mangling")
}

func TestExtractGoldenEnhancedWebP(t *testing.T) {
	data := loadFixture(t, "enhanced.webp")

	api, _, found := extractEmbeddedWorkflows(data)

	require.True(t, found)

	md, ok := ExtractMetadata(api)
	require.True(t, ok)

	assert.Equal(t, "shrew walkin down main street", md.OriginalPrompt)
	assert.False(t, md.LLMGenerated)
	assert.Equal(t, "ed974b6d", md.JobID)
	require.NotEmpty(t, md.Reasoning)
	assert.Len(t, md.Reasoning, 677, "reasoning length pinned from the production file")
	assert.True(t, strings.HasPrefix(md.Reasoning, "The user described a shrew walking down Main Street.\n\nExpanding the shrew scene"), "reasoning decodes with real newlines")
	assert.True(t, strings.HasSuffix(md.Reasoning, "\"expanded the prompt\"."), "reasoning ends with the documented tail (escaped quotes decode)")
	assert.Equal(t, fixtureEnhanced528, md.EnhancedPrompt)
	assert.Empty(t, md.NegativePrompt)
	require.NotNil(t, md.Seed)
	assert.EqualValues(t, 5702895218061442231, *md.Seed)
	require.NotNil(t, md.Steps)
	assert.Equal(t, 8, *md.Steps)
	assert.Equal(t, "euler", md.Sampler)
	assert.Equal(t, "krea2_turbo_int8_convrot.safetensors", md.ModelUnet)
	require.NotNil(t, md.Width)
	assert.Equal(t, 1920, *md.Width)
	require.NotNil(t, md.Height)
	assert.Equal(t, 1080, *md.Height)
}

// ─── Adversarial / format-variant cases ───────────────────────────────────

func TestExtractLittleEndianTIFF(t *testing.T) {
	payload := `{"58":{"class_type":"KSampler","inputs":{"seed":7,"steps":2,"cfg":1}}}`
	webp := buildWebP(buildTIFF("prompt:"+payload+"\x00", true), true)

	api, _, found := extractEmbeddedWorkflows(webp)

	require.True(t, found)
	assert.Contains(t, api, "KSampler")
}

func TestExtractWorkflowOnlyPayloadIgnored(t *testing.T) {
	payload := `{"3":{"class_type":"KSampler","inputs":{"seed":1}}}`
	webp := buildWebP(buildTIFF("workflow:"+payload+"\x00", false), true)

	api, _, found := extractEmbeddedWorkflows(webp)

	assert.Empty(t, api)
	assert.False(t, found, "a UI graph alone is not extraction input")
}

func TestExtractTruncatedEXIF(t *testing.T) {
	tiff := buildTIFF("prompt:"+`{"a":1}`+"\x00", false)
	webp := buildWebP(tiff[:12], true) // cut mid-header/IFD

	_, _, found := extractEmbeddedWorkflows(webp)

	assert.False(t, found, "truncated EXIF must yield not-found, not a panic")
}

func TestExtractNoEXIFChunk(t *testing.T) {
	webp := buildWebP(nil, false)

	_, _, found := extractEmbeddedWorkflows(webp)

	assert.False(t, found)
}

func TestExtractOddSizeChunkPadding(t *testing.T) {
	tiff := buildTIFF("prompt:"+`{"58":{"class_type":"KSampler","inputs":{"seed":7}}}`+"\x00", false)
	// Hand-rolled container: VP8X, then an odd-sized chunk with its
	// mandatory pad byte, THEN the EXIF chunk. A walker that ignores the
	// pad misaligns and never reaches the EXIF.
	var body bytes.Buffer
	body.WriteString("WEBP")
	body.WriteString("VP8X\x0a\x00\x00\x00")
	body.Write(make([]byte, 10))
	body.WriteString("ICCP\x03\x00\x00\x00xyz") // 3-byte (odd) body
	body.WriteByte(0)                           // pad byte
	body.WriteString("EXIF")
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], uint32(len(tiff)))
	body.Write(sz[:])
	body.Write(tiff)

	var total [4]byte
	binary.LittleEndian.PutUint32(total[:], uint32(body.Len()))
	webp := append([]byte("RIFF"), total[:]...)
	webp = append(webp, body.Bytes()...)

	api, _, found := extractEmbeddedWorkflows(webp)

	require.True(t, found, "EXIF after a padded odd-size chunk must be found")
	assert.Contains(t, api, `"seed":7`)
}

func TestExtractCorruptRIFF(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"Empty", nil},
		{"TooShort", []byte("RI")},
		{"NotRIFF", []byte("JUNKJUNKJUNKJUNKJUNK")},
		{"RIFFNotWebP", []byte("RIFF\x04\x00\x00\x00WAVEfmt ")},
		{"SizePastEOF", []byte("RIFF\xff\xff\xff\xffWEBPVP8X\x0a\x00\x00\x00abcdefghij")},
		{"ChunkSizePastEOF", []byte("RIFF\x18\x00\x00\x00WEBPEXIF\xff\x00\x00\x00ab")},
		{"TruncatedAfterEXIFHeader", []byte("RIFF\x10\x00\x00\x00WEBPEXIF\x04\x00\x00\x00MM\x00")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, found := extractEmbeddedWorkflows(tt.data)
			assert.False(t, found, "corrupt input must degrade to not-found, never panic")
		})
	}
}

func TestExtractPNGText(t *testing.T) {
	payload := `{"58":{"class_type":"EmptyLatentImage","inputs":{"width":8,"height":8}}}`
	png := buildPNG("prompt", payload)

	api, _, found := extractEmbeddedWorkflows(png)

	require.True(t, found)
	assert.JSONEq(t, payload, api)

	md, ok := ExtractMetadata(api)
	require.True(t, ok)
	require.NotNil(t, md.Width)
	assert.Equal(t, 8, *md.Width)
}

func TestExtractPNGOtherKeywordIgnored(t *testing.T) {
	png := buildPNG("comment", "not a workflow")

	_, _, found := extractEmbeddedWorkflows(png)

	assert.False(t, found)
}

func TestExtractExifPrefixTolerance(t *testing.T) {
	payload := `{"58":{"class_type":"KSampler","inputs":{"seed":9}}}`
	// Some writers put "Exif\x00\x00" between the chunk body and the TIFF
	// header; tolerated per the EXIF-in-webp convention.
	tiff := append([]byte("Exif\x00\x00"), buildTIFF("prompt:"+payload+"\x00", false)...)
	webp := buildWebP(tiff, true)

	api, _, found := extractEmbeddedWorkflows(webp)

	require.True(t, found)
	assert.Contains(t, api, `"seed":9`)
}

func TestExtractNonImageBytes(t *testing.T) {
	_, _, found := extractEmbeddedWorkflows([]byte("plain text, nothing embedded"))
	assert.False(t, found)
}
