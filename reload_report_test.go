package main

import (
	"testing"

	"github.com/rivo/tview"
	"github.com/stretchr/testify/assert"
)

func TestReloadReport_PrintNoErrors(t *testing.T) {
	r := NewReloadReport("reload")
	r.AddSuccess("config: 3 services, 5 chats")
	r.AddSuccess("ignores: 10 patterns")

	view := tview.NewTextView()
	r.Print(view)
	got := view.GetText(true)

	assert.Contains(t, got, "── reload ok ──")
	assert.Contains(t, got, "config: 3 services, 5 chats")
	assert.Contains(t, got, "ignores: 10 patterns")
	assert.NotContains(t, got, "── errors ──")
}

func TestReloadReport_PrintWithErrors(t *testing.T) {
	r := NewReloadReport("startup")
	r.AddSuccess("config: 2 services")
	r.AddError("MCP brave-mcp: connection refused")
	r.AddError("MCP fetch-mcp: timeout")

	view := tview.NewTextView()
	r.Print(view)
	got := view.GetText(true)

	assert.Contains(t, got, "── startup completed with 2 errors ──")
	assert.Contains(t, got, "config: 2 services")
	assert.Contains(t, got, "── 2 errors ──")
	assert.Contains(t, got, "MCP brave-mcp: connection refused")
	assert.Contains(t, got, "MCP fetch-mcp: timeout")
}

func TestReloadReport_PrintEmpty(t *testing.T) {
	r := NewReloadReport("reload")

	view := tview.NewTextView()
	r.Print(view)
	got := view.GetText(true)

	assert.Contains(t, got, "── reload ok ──")
	assert.NotContains(t, got, "── errors ──")
}

func TestReloadReport_HasErrors(t *testing.T) {
	r := NewReloadReport("reload")
	assert.False(t, r.HasErrors())
	r.AddError("something broke")
	assert.True(t, r.HasErrors())
}

func TestReloadReport_PrintSingleError(t *testing.T) {
	r := NewReloadReport("reload")
	r.AddSuccess("config: ok")
	r.AddError("MCP x: failed")

	view := tview.NewTextView()
	r.Print(view)
	got := view.GetText(true)

	assert.Contains(t, got, "── reload completed with 1 error ──")
	assert.Contains(t, got, "── 1 error ──")
	assert.NotContains(t, got, "1 errors")
}
