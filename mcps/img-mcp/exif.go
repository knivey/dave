package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
)

// Container-level scanning for embedded ComfyUI workflows (read side).
//
// webp: a RIFF walk to the EXIF chunk (raw TIFF), then an IFD0 walk for
// ASCII (type 2) entries whose payload is "prompt:" + API-workflow-JSON +
// NUL (plus a sibling "workflow:" UI graph which is detected but ignored
// here — img-mcp only needs the API graph). png: tEXt chunks keyed
// "prompt" — ComfyUI's standard SaveImage convention.
//
// This is img-mcp's read-side twin of imgsite's extractor (same
// production-verified formats; see imgsite/extract.go). The EXIF note
// rewrite (post-generation verdict/provenance surgery) extends this file
// with the write side.
//
// All functions are pure and defensive: malformed input yields no result,
// never a panic. Extraction failure degrades upstream — it must never fail
// a job.

// tiffTypeASCII is the TIFF field type whose IFD0 entries carry the
// "prompt:"-prefixed workflow payload. The production reader does NOT filter
// by tag number (it scans every ASCII entry — production files carry the
// payload in the Model tag, 0x0110, but the extraction is tag-agnostic), so
// no tag constants live here.
const tiffTypeASCII = 2

// embeddedWorkflowJSON returns the API-format workflow JSON embedded in a
// completed image's bytes. found is true only when a "prompt" payload was
// located.
func embeddedWorkflowJSON(data []byte) (string, bool) {
	if exif, ok := webpEXIFChunk(data); ok {
		// Some writers prefix the TIFF with "Exif\x00\x00"; tolerated per
		// the EXIF-in-webp convention.
		exif = bytes.TrimPrefix(exif, []byte("Exif\x00\x00"))
		for _, p := range tiffASCIIPayloads(exif) {
			p = strings.TrimRight(p, "\x00")
			if strings.HasPrefix(p, "prompt:") {
				return strings.TrimPrefix(p, "prompt:"), true
			}
		}
		return "", false
	}
	if texts := pngTextChunks(data); len(texts) > 0 {
		// PNG tEXt values are raw JSON, not "prompt:"-prefixed.
		if api := texts["prompt"]; api != "" {
			return api, true
		}
	}
	return "", false
}

// webpEXIFChunk walks the RIFF chunk list of a webp file and returns the
// raw TIFF bytes of the first EXIF chunk. Chunk layout per the WebP
// container spec: fourcc + little-endian uint32 size + body, with a single
// pad byte after odd-sized bodies (the production EXIF chunk is odd-sized,
// so the padding rule is load-bearing).
//
// All offset/size arithmetic uses int64 intermediates: chunk sizes are
// attacker-controlled uint32s, and on 32-bit platforms plain int addition
// of two large values could wrap past a small len(data) check.
func webpEXIFChunk(data []byte) ([]byte, bool) {
	if len(data) < 12 || !bytes.Equal(data[0:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return nil, false
	}
	off := int64(12)
	for off+8 <= int64(len(data)) {
		fourcc := data[off : off+4]
		size := int64(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		bodyStart := off + 8
		if bodyStart+size > int64(len(data)) {
			// Corrupt chunk header (size past EOF): nothing trustworthy
			// follows, including any EXIF.
			return nil, false
		}
		if string(fourcc) == "EXIF" {
			return data[bodyStart : bodyStart+size], true
		}
		off = bodyStart + size + (size & 1)
	}
	return nil, false
}

// pngTextChunks collects tEXt chunks (keyword\0value) from a PNG stream.
// int64 offset arithmetic for the same 32-bit-wrap reason as
// webpEXIFChunk.
func pngTextChunks(data []byte) map[string]string {
	pngSig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if len(data) < 8 || !bytes.Equal(data[0:8], pngSig) {
		return nil
	}
	out := make(map[string]string)
	off := int64(8)
	for off+8 <= int64(len(data)) {
		size := int64(binary.BigEndian.Uint32(data[off : off+4]))
		ctype := string(data[off+4 : off+8])
		bodyStart := off + 8
		if bodyStart+size > int64(len(data)) {
			// Truncated chunk: stop scanning.
			break
		}
		body := data[bodyStart : bodyStart+size]
		if ctype == "tEXt" {
			if nul := bytes.IndexByte(body, 0); nul >= 0 {
				out[string(body[:nul])] = string(body[nul+1:])
			}
		}
		if ctype == "IEND" {
			break
		}
		off = bodyStart + size + 4 // PNG chunks carry a 4-byte CRC after the body
	}
	return out
}

// tiffTypeSize maps TIFF field types to their element size in bytes.
var tiffTypeSize = map[uint16]int{
	1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 6: 1, 7: 1, 8: 2, 9: 4, 10: 8, 11: 4, 12: 8,
}

// tiffASCIIPayloads walks IFD0 of a TIFF blob and returns the raw bytes of
// every ASCII (type 2) entry, most importantly the Model (0x0110) tag
// where ComfyUI's webp writer stashes the workflow. Production files carry
// exactly one IFD0 entry; we do not chase the IFD chain or the Exif
// sub-IFD — a camera-style EXIF block has no "prompt:" payloads anyway,
// and bounded walking keeps adversarial inputs cheap.
//
// Known exotic-input envelope: TIFF says field values SHOULD start on a
// word (even) boundary, but this reader is strictly offset-driven — it
// reads at whatever offset the entry declares, aligned or not, and never
// writes — so mis-aligned values from exotic writers still parse. Sub-IFD
// contents (ExifIFD/GPS) are likewise invisible here: only the pointer
// entry in IFD0 is seen, as an opaque non-ASCII entry that is skipped.
//
// As with the container walkers, offsets and sizes are computed in int64:
// field counts and value offsets are attacker-influenced uint32s whose
// sums could wrap a 32-bit int past the length check.
func tiffASCIIPayloads(t []byte) []string {
	if len(t) < 8 {
		return nil
	}
	var bo binary.ByteOrder
	switch {
	case bytes.Equal(t[0:2], []byte("MM")):
		bo = binary.BigEndian
	case bytes.Equal(t[0:2], []byte("II")):
		bo = binary.LittleEndian
	default:
		return nil
	}
	if bo.Uint16(t[2:4]) != 42 {
		return nil
	}
	ifdOff := int64(bo.Uint32(t[4:8]))
	if ifdOff+2 > int64(len(t)) {
		return nil
	}
	entryCount := int64(bo.Uint16(t[ifdOff : ifdOff+2]))
	var payloads []string
	entriesStart := ifdOff + 2
	for i := int64(0); i < entryCount; i++ {
		e := entriesStart + i*12
		if e+12 > int64(len(t)) {
			break // truncated IFD: keep whatever parsed so far
		}
		typ := bo.Uint16(t[e+2 : e+4])
		count := int64(bo.Uint32(t[e+4 : e+8]))
		if typ != tiffTypeASCII {
			continue
		}
		size := int64(tiffTypeSize[typ]) * count
		if size <= 0 {
			continue
		}
		var raw []byte
		if size <= 4 {
			raw = t[e+8 : e+8+size]
		} else {
			valOff := int64(bo.Uint32(t[e+8 : e+12]))
			if valOff+size > int64(len(t)) {
				continue // offset out of bounds: skip entry
			}
			raw = t[valOff : valOff+size]
		}
		payloads = append(payloads, string(raw))
	}
	return payloads
}

// enhancedPromptFromImage reads the enhanced prompt out of the workflow
// embedded in a completed image. On the restart-recovery path this is the
// only surviving copy — recovery never re-runs enhancement, so the prompt
// the image actually ran with lives solely in the file ComfyUI produced.
// The prompt node is located via the workflow config exactly as
// prepareComfyWorkflow located it at submit time. ok is false when no
// embedded workflow or prompt text could be recovered; callers degrade to
// vetting the original prompt only.
func enhancedPromptFromImage(cfg Config, workflowName string, data []byte) (string, bool) {
	wc, ok := cfg.Workflows[workflowName]
	if !ok {
		return "", false
	}
	wfJSON, ok := embeddedWorkflowJSON(data)
	if !ok {
		return "", false
	}
	var workflow ComfyWorkflow
	if err := json.Unmarshal([]byte(wfJSON), &workflow); err != nil {
		return "", false
	}
	node, ok := workflow[wc.PromptNode]
	if !ok || node.Inputs == nil {
		return "", false
	}
	text, ok := node.Inputs["text"].(string)
	if !ok || text == "" {
		return "", false
	}
	return text, true
}

// embeddedPromptNote decodes the dave_original_prompt note payload from the
// workflow embedded in a completed image. This is the write side's read
// mirror: the EXIF note rewrite replaces the note wholesale, so the recovery
// path reads the old payload first to carry fields it cannot rebuild (the
// enhancement reasoning — recovery never re-runs enhancement, so the note is
// its only surviving copy) across the rewrite. ok is false when no note could
// be recovered; callers degrade to empty.
func embeddedPromptNote(data []byte) (promptNotePayload, bool) {
	wfJSON, ok := embeddedWorkflowJSON(data)
	if !ok {
		return promptNotePayload{}, false
	}
	// UseNumber keeps large int64 seeds exact through the round trip; the
	// note lives one level deeper (a string field), but parsing the workflow
	// through map[string]any is the only shape-agnostic walk available and
	// costs nothing here (the bytes are re-marshaled by the rewrite, not by
	// this reader — the values below come from the note's own JSON).
	dec := json.NewDecoder(strings.NewReader(wfJSON))
	dec.UseNumber()
	var workflow map[string]any
	if err := dec.Decode(&workflow); err != nil {
		return promptNotePayload{}, false
	}
	node, ok := workflow[davePromptNoteNodeID].(map[string]any)
	if !ok {
		return promptNotePayload{}, false
	}
	inputs, ok := node["inputs"].(map[string]any)
	if !ok {
		return promptNotePayload{}, false
	}
	text, ok := inputs["text"].(string)
	if !ok {
		return promptNotePayload{}, false
	}
	var payload promptNotePayload
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return promptNotePayload{}, false
	}
	return payload, true
}
