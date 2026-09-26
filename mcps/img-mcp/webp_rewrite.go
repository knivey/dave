package main

// EXIF note rewrite (write side of exif.go's container scanning).
//
// The post-generation, pre-upload stage that bakes the resolved safety
// verdict and IRC provenance into the completed image itself: RIFF container
// surgery that replaces the dave_original_prompt note node's text INSIDE the
// workflow JSON carried by the webp EXIF chunk, leaving every other chunk
// and all image bytes byte-identical. The image is never re-encoded — only
// the metadata payload it already carries is enriched, so pixels and every
// sibling chunk survive untouched.
//
// Layout knowledge mirrors exif.go/imgsite's production-verified readers:
//
//	RIFF <size> WEBP
//	  VP8X <size> <body> [pad]        ── image/container chunks, preserved
//	  EXIF <size> [Exif\0\0] <TIFF>   ── the chunk carrying the workflow
//	  ...                             ── later chunks, preserved
//
// and inside the TIFF, IFD0 holds ASCII (type 2) entries; the workflow rides
// one whose value is "prompt:" + API-workflow-JSON + NUL padding (production
// files: exactly one Model-tag entry). The rewrite re-encodes the TIFF in the
// same shape — same byte order, same entry order and tags, value area
// re-laid-out contiguously after the IFD — with only the prompt entry's
// payload replaced.
//
// Ordering at the call sites is load-bearing: rewrite → upload, so the bytes
// imgsite receives (and hashes for dedup) are the enriched bytes. See
// processJob/recoverRunningJob in queue.go.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errWebpOnly is the sentinel for "this file is not a RIFF/WEBP container".
// Production output is webp (Image Saver Simple, embed_workflow=true); a
// non-webp output cannot take the note rewrite, and callers WARN and skip on
// this sentinel — the safety verdict still travels in the upload meta.
var errWebpOnly = errors.New("not a webp container: the EXIF note rewrite supports webp output only")

// promptPayloadPrefix is the marker ComfyUI's writer (and imgsite's reader)
// use to identify the API-workflow JSON inside an EXIF ASCII entry.
const promptPayloadPrefix = "prompt:"

// exifHeaderPrefix is the optional "Exif\x00\x00" some writers put between
// the EXIF chunk body and the TIFF; tolerated on read (exif.go), preserved
// verbatim on write.
var exifHeaderPrefix = []byte("Exif\x00\x00")

// riffChunk is one parsed WebP container chunk: fourcc + little-endian u32
// size + body (+ one pad byte after odd-sized bodies, applied by the
// assembler, never part of body).
type riffChunk struct {
	fourcc string
	body   []byte
}

// isWebpContainer reports whether data starts with a RIFF/WEBP header.
func isWebpContainer(data []byte) bool {
	return len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP"))
}

// parseWebpChunks splits a webp file into its chunk list. Bytes after the
// last complete chunk (a run shorter than a chunk header) are returned as
// trailing so the assembler can preserve them; a chunk header whose size
// runs past EOF is corrupt and errors — the rewrite must never silently
// drop bytes it declined to understand. int64 arithmetic throughout for the
// same 32-bit-wrap reason as exif.go's walkers.
func parseWebpChunks(data []byte) (chunks []riffChunk, trailing []byte, err error) {
	if !isWebpContainer(data) {
		return nil, nil, errWebpOnly
	}
	off := int64(12)
	for off+8 <= int64(len(data)) {
		size := int64(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		bodyStart := off + 8
		if bodyStart+size > int64(len(data)) {
			return nil, nil, fmt.Errorf("corrupt webp chunk %q at offset %d: size %d runs past EOF",
				string(data[off:off+4]), off, size)
		}
		chunks = append(chunks, riffChunk{
			fourcc: string(data[off : off+4]),
			body:   data[bodyStart : bodyStart+size],
		})
		off = bodyStart + size + (size & 1)
	}
	return chunks, data[off:], nil
}

// buildWebp reassembles a webp container from chunks (plus any trailing
// bytes parseWebpChunks found), recomputing the RIFF size and the per-chunk
// odd-size pad bytes. Non-target chunk bodies are copied verbatim by the
// caller, so this never changes their bytes — only the framing around them.
func buildWebp(chunks []riffChunk, trailing []byte) []byte {
	body := []byte("WEBP")
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
	body = append(body, trailing...)

	out := make([]byte, 0, 8+len(body))
	out = append(out, []byte("RIFF")...)
	var total [4]byte
	binary.LittleEndian.PutUint32(total[:], uint32(len(body)))
	out = append(out, total[:]...)
	return append(out, body...)
}

// replacePromptNoteInWorkflowJSON returns wfJSON with the note node's text
// replaced by noteJSON. The workflow is decoded via json.Number (not
// float64) so int64 seed values — rand.Int63() spans up to 2^63-1, beyond
// float64's exact integer range — re-encode as their exact original literals
// instead of silently corrupting; the embedded metadata must keep matching
// the generation it accompanied. Key order is not preserved (Go marshals
// maps sorted); every reader parses the JSON, so order was never contractual.
func replacePromptNoteInWorkflowJSON(wfJSON, noteJSON string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(wfJSON))
	dec.UseNumber()
	var workflow map[string]any
	if err := dec.Decode(&workflow); err != nil {
		return "", fmt.Errorf("parsing embedded workflow: %w", err)
	}
	node, ok := workflow[davePromptNoteNodeID].(map[string]any)
	if !ok {
		return "", fmt.Errorf("note node %q missing from embedded workflow", davePromptNoteNodeID)
	}
	inputs, ok := node["inputs"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("note node %q has no inputs map", davePromptNoteNodeID)
	}
	inputs["text"] = noteJSON
	out, err := json.Marshal(workflow)
	if err != nil {
		return "", fmt.Errorf("marshaling rewritten workflow: %w", err)
	}
	return string(out), nil
}

// rewriteTIFFPromptValue rebuilds the EXIF TIFF with the first
// "prompt:"-prefixed ASCII IFD0 entry carrying the enriched note. The output
// keeps the input's shape: byte order, header/gap bytes before IFD0, entry
// count, entry order, tags, types, and inline values are preserved verbatim;
// out-of-line values are re-laid-out contiguously after the IFD in entry
// order (only the prompt entry's length changes, but recomputing every
// offset is simpler and just as correct), and any unparsed trailing bytes
// after the last referenced value are appended verbatim.
//
// Refused (error → caller WARNs and skips, original bytes flow on): chained
// IFDs (a non-zero next-IFD pointer means offsets elsewhere may point into
// the area being re-laid-out), out-of-bounds values, unknown field types
// with data (their size is unknowable, so faithful preservation is too), and
// a missing prompt entry. Production files are one-entry, next-IFD-zero
// TIFFs, so none of these occur in practice — they are tripwires against
// mangling exotic inputs.
func rewriteTIFFPromptValue(t []byte, noteJSON string) ([]byte, error) {
	if len(t) < 8 {
		return nil, fmt.Errorf("exif TIFF too short (%d bytes)", len(t))
	}
	var bo binary.ByteOrder
	switch {
	case bytes.Equal(t[0:2], []byte("MM")):
		bo = binary.BigEndian
	case bytes.Equal(t[0:2], []byte("II")):
		bo = binary.LittleEndian
	default:
		return nil, fmt.Errorf("exif TIFF has no byte-order mark")
	}
	if bo.Uint16(t[2:4]) != 42 {
		return nil, fmt.Errorf("exif TIFF magic is not 42")
	}
	ifdOff := int64(bo.Uint32(t[4:8]))
	if ifdOff < 8 || ifdOff+2 > int64(len(t)) {
		return nil, fmt.Errorf("IFD0 offset %d out of bounds", ifdOff)
	}
	entryCount := int64(bo.Uint16(t[ifdOff : ifdOff+2]))
	ifdEnd := ifdOff + 2 + entryCount*12 + 4 // entries + next-IFD pointer
	if ifdEnd > int64(len(t)) {
		return nil, fmt.Errorf("IFD0 truncated: %d entries need %d bytes, TIFF is %d", entryCount, ifdEnd-2, len(t))
	}
	if next := bo.Uint32(t[ifdOff+2+entryCount*12 : ifdEnd]); next != 0 {
		return nil, fmt.Errorf("chained IFDs are not supported by the note rewrite (next-IFD pointer %d)", next)
	}

	type entry struct {
		tag, typ uint16
		count    uint32
		// inline holds the entry's original 4 value-area bytes verbatim
		// (value + padding) when the value is stored inline (size <= 4);
		// otherwise value holds the referenced bytes.
		inline   [4]byte
		value    []byte
		isInline bool
	}

	entries := make([]entry, 0, entryCount)
	target := -1
	var trailingNULs int
	maxEnd := ifdEnd // end of the last referenced out-of-line value
	for i := int64(0); i < entryCount; i++ {
		e := ifdOff + 2 + i*12
		ent := entry{
			tag:   bo.Uint16(t[e : e+2]),
			typ:   bo.Uint16(t[e+2 : e+4]),
			count: bo.Uint32(t[e+4 : e+8]),
		}
		size := int64(tiffTypeSize[ent.typ]) * int64(ent.count)
		if ent.typ != tiffTypeASCII && ent.count > 0 && tiffTypeSize[ent.typ] == 0 {
			return nil, fmt.Errorf("IFD0 entry %d has unknown field type %d; refusing to rewrite", i, ent.typ)
		}
		if size <= 4 {
			copy(ent.inline[:], t[e+8:e+12])
			ent.isInline = true
			// The NUL-padded inline bytes are not a prompt payload
			// ("prompt:" alone exceeds 4 bytes), so no target match here.
		} else {
			valOff := int64(bo.Uint32(t[e+8 : e+12]))
			if valOff+size > int64(len(t)) {
				return nil, fmt.Errorf("IFD0 entry %d value at %d (size %d) out of bounds", i, valOff, size)
			}
			ent.value = t[valOff : valOff+size]
			if valOff+size > maxEnd {
				maxEnd = valOff + size
			}
		}
		if target < 0 && ent.typ == tiffTypeASCII && !ent.isInline &&
			bytes.HasPrefix(ent.value, []byte(promptPayloadPrefix)) {
			target = int(i)
			trimmed := bytes.TrimRight(ent.value, "\x00")
			trailingNULs = len(ent.value) - len(trimmed)
		}
		entries = append(entries, ent)
	}
	if target < 0 {
		return nil, fmt.Errorf("no %q payload found in the EXIF chunk", promptPayloadPrefix)
	}

	// The new payload: same framing as found — "prompt:" prefix and the
	// original NUL pad count — around the enriched workflow JSON.
	trimmed := bytes.TrimRight(entries[target].value, "\x00")
	oldJSON := string(trimmed[len(promptPayloadPrefix):])
	newJSON, err := replacePromptNoteInWorkflowJSON(oldJSON, noteJSON)
	if err != nil {
		return nil, err
	}
	newVal := make([]byte, 0, len(promptPayloadPrefix)+len(newJSON)+trailingNULs)
	newVal = append(newVal, promptPayloadPrefix...)
	newVal = append(newVal, newJSON...)
	newVal = append(newVal, bytes.Repeat([]byte{0}, trailingNULs)...)
	entries[target].count = uint32(len(newVal))
	entries[target].value = newVal

	// Lay out the rebuilt TIFF: header+gap verbatim, then the IFD with
	// recomputed value offsets, then the out-of-line values in entry order,
	// then whatever trailing bytes the original carried.
	out := make([]byte, 0, len(t)+len(newJSON))
	out = append(out, t[:ifdOff]...)
	out = append(out, t[ifdOff:ifdOff+2]...) // entry count is unchanged
	cursor := ifdEnd
	offsets := make([]int64, len(entries))
	for i := range entries {
		if !entries[i].isInline {
			offsets[i] = cursor
			cursor += int64(len(entries[i].value))
		}
	}
	for i := range entries {
		var rec [12]byte
		bo.PutUint16(rec[0:2], entries[i].tag)
		bo.PutUint16(rec[2:4], entries[i].typ)
		bo.PutUint32(rec[4:8], entries[i].count)
		if entries[i].isInline {
			copy(rec[8:12], entries[i].inline[:])
		} else {
			bo.PutUint32(rec[8:12], uint32(offsets[i]))
		}
		out = append(out, rec[:]...)
	}
	out = append(out, t[ifdOff+2+entryCount*12:ifdEnd]...) // next-IFD zeros, verbatim
	for i := range entries {
		if !entries[i].isInline {
			out = append(out, entries[i].value...)
		}
	}
	out = append(out, t[maxEnd:]...)
	return out, nil
}

// rewriteWebpNoteData is the in-memory form of the note rewrite: data with
// the EXIF chunk's embedded workflow re-written so the dave_original_prompt
// node's text is noteJSON. Every chunk except the EXIF (and the RIFF/chunk
// size framing its new length forces) is byte-identical; the pad-byte and
// "Exif\0\0" conventions found in the input are reproduced. This is what the
// upload path calls — the image bytes imgsite receives and hashes are these
// bytes.
func rewriteWebpNoteData(data []byte, noteJSON string) ([]byte, error) {
	chunks, trailing, err := parseWebpChunks(data)
	if err != nil {
		return nil, err
	}
	exifIdx := -1
	for i, c := range chunks {
		if c.fourcc == "EXIF" {
			exifIdx = i
			break
		}
	}
	if exifIdx < 0 {
		return nil, fmt.Errorf("webp has no EXIF chunk to rewrite")
	}

	body := chunks[exifIdx].body
	hadPrefix := bytes.HasPrefix(body, exifHeaderPrefix)
	tiff := bytes.TrimPrefix(body, exifHeaderPrefix)
	newTIFF, err := rewriteTIFFPromptValue(tiff, noteJSON)
	if err != nil {
		return nil, err
	}
	if hadPrefix {
		prefixed := make([]byte, 0, len(exifHeaderPrefix)+len(newTIFF))
		prefixed = append(prefixed, exifHeaderPrefix...)
		chunks[exifIdx].body = append(prefixed, newTIFF...)
	} else {
		chunks[exifIdx].body = newTIFF
	}
	return buildWebp(chunks, trailing), nil
}

// rewriteNoteInWebp rewrites the note inside the webp file at path: the
// file-based twin of rewriteWebpNoteData, kept as the durable entry point for
// tooling/tests that operate on files. The write is atomic — a temp file in
// the same directory (rename across filesystems would not be) followed by
// rename — so a crash mid-rewrite can never leave a half-written image; the
// original mode is preserved. A non-webp file returns errWebpOnly for the
// caller to WARN and skip.
func rewriteNoteInWebp(path string, noteJSON string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading image for note rewrite: %w", err)
	}
	out, err := rewriteWebpNoteData(data, noteJSON)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".dave-note-rewrite-*")
	if err != nil {
		return fmt.Errorf("creating temp file for note rewrite: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing rewritten image: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing rewritten image: %w", err)
	}
	if fi, statErr := os.Stat(path); statErr == nil {
		// Best-effort: CreateTemp uses 0600, gallery files are 0644.
		_ = os.Chmod(tmpName, fi.Mode().Perm())
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("replacing image with the rewritten copy: %w", err)
	}
	return nil
}
