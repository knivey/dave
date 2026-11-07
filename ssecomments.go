package main

import (
	"bytes"
	"io"
)

// SSE comment filtering (Nov 2026; prod incidents 2026-11-04/05/06 —
// sessions 2890, 2908, 3002, 3007 ×2, root cause relay-captured live):
//
// llama.cpp's server emits a spec-legal SSE keepalive COMMENT (":\n\n")
// after every 30s of stream silence. For a streaming chat completion that
// silence is prompt processing: a cold-cache ~21k-token prefill takes
// ~30.5s with zero SSE bytes flowing, so the keepalive lands mid-prefill.
// The openai-go SDK's ssestream (v3.33.0 packages/ssestream) correctly
// ignores the comment LINE but then dispatches the comment's trailing
// BLANK LINE as a data-less event, and Stream.Next() runs
// json.Unmarshal([]byte(""), ...) → "unexpected end of JSON input" → the
// whole stream aborts mid-prefill and the user sees a cryptic error.
// Warm-cache turns prefill in ~1s and never hit the 30s mark, which is
// why only the FIRST big request after a model load died (the KV cache
// makes every later turn fast), why curl never "failed" (it does not
// parse SSE), and why the webui never died (proper SSE parsers ignore
// comments — the owner's original suspicion).
//
// daveTransport therefore strips, from every text/event-stream response
// body BEFORE the SDK sees it:
//   - comment lines (':' prefix, including bare ':')
//   - blank lines that would dispatch a DATA-LESS event (a blank line
//     when no field line accumulated since the previous blank line)
//
// Every other byte passes through unchanged (including CRLF terminators
// and multi-line data fields), so well-formed streams are byte-identical.
// Residual, documented: any dispatch carrying NO data: field line would
// still poison the SDK — e.g. an event:-only or id:-only event
// ("event: foo\n\n") passes the filter (a field line armed the blank) but
// dispatches empty data — no server we use emits those shapes.
// NOTE: the filter changes body byte offsets relative to the wire — any
// future reattach (GET /v1/stream?from=N) must count offsets on the
// FILTERED byte stream, not the raw one.

type sseCommentFilter struct {
	rc       io.ReadCloser
	buf      []byte // scratch for upstream reads (hoisted; consume copies what it keeps)
	pending  []byte // decoded bytes ready to deliver
	partial  []byte // current incomplete line (terminator not yet seen)
	sawField bool   // a field line is buffered since the last blank line
	eof      bool
	err      error
}

func newSSECommentFilter(rc io.ReadCloser) *sseCommentFilter {
	return &sseCommentFilter{rc: rc, buf: make([]byte, 16*1024)}
}

func (f *sseCommentFilter) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		if f.eof {
			// Flush a trailing partial line unless it is a comment.
			if len(f.partial) > 0 {
				line := f.partial
				f.partial = nil
				if line[0] != ':' {
					f.pending = line
					continue
				}
			}
			if f.err != nil {
				return 0, f.err
			}
			return 0, io.EOF
		}
		buf := f.buf
		n, err := f.rc.Read(buf)
		if n > 0 {
			f.consume(buf[:n])
		}
		if err != nil {
			f.eof = true
			f.err = err // non-EOF errors surface after the flush
		}
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

func (f *sseCommentFilter) Close() error {
	return f.rc.Close()
}

// consume classifies complete lines; a line is blank when it is empty or
// a bare "\r" (CRLF blank). Comment lines and data-less blank lines are
// dropped; everything else is passed through with its original bytes
// (line content plus the '\n' terminator — a CRLF line keeps its '\r'
// inside the content).
func (f *sseCommentFilter) consume(data []byte) {
	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			f.partial = append(f.partial, data...)
			return
		}
		line := append(f.partial, data[:idx]...)
		f.partial = f.partial[:0]
		term := data[idx : idx+1]
		data = data[idx+1:]

		content := line
		if len(content) > 0 && content[len(content)-1] == '\r' {
			content = content[:len(content)-1]
		}
		switch {
		case len(content) == 0:
			// Blank line: event dispatch point. Pass it through only when
			// it would dispatch a real event; a data-less dispatch is the
			// SDK poison this filter exists to remove.
			if f.sawField {
				f.pending = append(f.pending, line...)
				f.pending = append(f.pending, term...)
				f.sawField = false
			}
		case content[0] == ':':
			// SSE comment — drop.
		default:
			f.pending = append(f.pending, line...)
			f.pending = append(f.pending, term...)
			f.sawField = true
		}
	}
}
