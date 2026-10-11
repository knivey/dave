package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// apiLogEntry matches dave's api_logs envelope exactly — the controller's
// existing analysis tooling parses this shape, do not deviate.
type apiLogEntry struct {
	Type string          `json:"type"`
	Ts   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

// jsonlWriter appends one JSONL line per HTTP exchange, flushed per line so
// a killed run leaves a parseable file behind.
type jsonlWriter struct {
	f *os.File
	w *bufio.Writer
}

func newJSONLWriter(path string) (*jsonlWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &jsonlWriter{f: f, w: bufio.NewWriter(f)}, nil
}

// logExchange writes one `{"type":...,"ts":...,"body":{...}}` line. Bodies
// that are not valid JSON (a proxy error page, say) are wrapped rather
// than corrupting the file.
func (j *jsonlWriter) logExchange(entryType string, body []byte) error {
	if !json.Valid(body) {
		wrapped, err := json.Marshal(map[string]string{"non_json_body": string(body)})
		if err != nil {
			return err
		}
		body = wrapped
	}
	entry := apiLogEntry{
		Type: entryType,
		Ts:   time.Now().UTC().Format(time.RFC3339Nano),
		Body: json.RawMessage(body),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := j.w.Write(data); err != nil {
		return err
	}
	return j.w.Flush()
}

func (j *jsonlWriter) Close() error {
	if err := j.w.Flush(); err != nil {
		return err
	}
	return j.f.Close()
}

// indexRow is one run-index.csv record: the per-session summary the
// controller's comparison scripts consume.
type indexRow struct {
	Variant        string
	ToolVariant    string
	Temperature    string
	Retries        int
	Scenario       string
	DeliveryRole   string
	Run            int
	Requests       int
	Responses      int
	ReasoningChars int
	Fabricated     bool
}

var indexHeader = []string{"variant", "tool_variant", "temperature", "retries", "scenario", "delivery_role", "run", "requests", "responses", "reasoning_chars_total", "fabricated"}

// runIndex streams run-index.csv rows; every add is flushed so partial
// sweeps still leave a readable index.
type runIndex struct {
	cw *csv.Writer
}

func newRunIndex(w io.Writer) *runIndex {
	cw := csv.NewWriter(w)
	_ = cw.Write(indexHeader)
	return &runIndex{cw: cw}
}

func (ri *runIndex) add(row indexRow) error {
	rec := []string{
		row.Variant,
		row.ToolVariant,
		row.Temperature,
		strconv.Itoa(row.Retries),
		row.Scenario,
		row.DeliveryRole,
		fmt.Sprintf("%02d", row.Run),
		strconv.Itoa(row.Requests),
		strconv.Itoa(row.Responses),
		strconv.Itoa(row.ReasoningChars),
		strconv.FormatBool(row.Fabricated),
	}
	if err := ri.cw.Write(rec); err != nil {
		return err
	}
	ri.cw.Flush()
	return ri.cw.Error()
}

func (ri *runIndex) close() error {
	ri.cw.Flush()
	return ri.cw.Error()
}
