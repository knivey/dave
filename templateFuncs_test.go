package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTableFunc(t *testing.T) {
	tests := []struct {
		name    string
		slice   any
		columns string
		want    string
		wantErr bool
	}{
		{
			name:    "NilSlice",
			slice:   nil,
			columns: "a,b",
			want:    "",
		},
		{
			name:    "EmptySlice",
			slice:   []any{},
			columns: "a,b",
			want:    "",
		},
		{
			name:    "NotSlice",
			slice:   "not a slice",
			columns: "a",
			wantErr: true,
		},
		{
			name: "SingleRow",
			slice: []any{
				map[string]any{"name": "alice", "age": float64(30)},
			},
			columns: "name,age",
			want:    "\n┌───────┬─────┐\n│ name  │ age │\n├───────┼─────┤\n│ alice │ 30  │\n└───────┴─────┘",
		},
		{
			name: "MultipleRows",
			slice: []any{
				map[string]any{"job_id": "abc", "status": "running"},
				map[string]any{"job_id": "def", "status": "done"},
			},
			columns: "job_id,status",
			want:    "\n┌────────┬─────────┐\n│ job_id │ status  │\n├────────┼─────────┤\n│ abc    │ running │\n│ def    │ done    │\n└────────┴─────────┘",
		},
		{
			name: "MissingField",
			slice: []any{
				map[string]any{"name": "alice"},
			},
			columns: "name,missing",
			want:    "\n┌───────┬─────────┐\n│ name  │ missing │\n├───────┼─────────┤\n│ alice │         │\n└───────┴─────────┘",
		},
		{
			name:    "ItemNotMap",
			slice:   []any{"not a map"},
			columns: "a",
			wantErr: true,
		},
		{
			name: "Float64Values",
			slice: []any{
				map[string]any{"eta": float64(56), "count": float64(0)},
			},
			columns: "eta,count",
			want:    "\n┌─────┬───────┐\n│ eta │ count │\n├─────┼───────┤\n│ 56  │ 0     │\n└─────┴───────┘",
		},
		{
			name:    "EmptyColumns",
			slice:   []any{map[string]any{"a": "b"}},
			columns: "",
			wantErr: true,
		},
		{
			name: "WhitespaceColumns",
			slice: []any{
				map[string]any{"name": "alice", "age": float64(30)},
			},
			columns: " name , age ",
			want:    "\n┌───────┬─────┐\n│ name  │ age │\n├───────┼─────┤\n│ alice │ 30  │\n└───────┴─────┘",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tableFunc(tt.slice, tt.columns)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
