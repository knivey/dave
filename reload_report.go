package main

import (
	"fmt"

	"github.com/rivo/tview"
)

type ReloadReport struct {
	label     string
	successes []string
	errors    []string
}

func NewReloadReport(label string) *ReloadReport {
	return &ReloadReport{label: label}
}

func (r *ReloadReport) AddSuccess(msg string) {
	r.successes = append(r.successes, msg)
}

func (r *ReloadReport) AddError(msg string) {
	r.errors = append(r.errors, msg)
}

func (r *ReloadReport) HasErrors() bool {
	return len(r.errors) > 0
}

func (r *ReloadReport) Print(view *tview.TextView) {
	if len(r.errors) == 0 {
		fmt.Fprintf(view, "[green]── %s ok ──[white]\n", r.label)
	} else {
		fmt.Fprintf(view, "[yellow]── %s completed with %d %s ──[white]\n",
			r.label, len(r.errors), pluralize(len(r.errors), "error"))
	}
	for _, s := range r.successes {
		fmt.Fprintf(view, "  %s\n", s)
	}
	if len(r.errors) > 0 {
		fmt.Fprintf(view, "[red]── %d %s ──[white]\n", len(r.errors), pluralize(len(r.errors), "error"))
		for _, e := range r.errors {
			fmt.Fprintf(view, "[red]  %s[white]\n", e)
		}
	}
}

func (r *ReloadReport) QueuePrint() {
	var lines []string
	if len(r.errors) == 0 {
		lines = append(lines, fmt.Sprintf("[green]── %s ok ──[white]", r.label))
	} else {
		lines = append(lines, fmt.Sprintf("[yellow]── %s completed with %d %s ──[white]",
			r.label, len(r.errors), pluralize(len(r.errors), "error")))
	}
	for _, s := range r.successes {
		lines = append(lines, "  "+s)
	}
	if len(r.errors) > 0 {
		lines = append(lines, fmt.Sprintf("[red]── %d %s ──[white]", len(r.errors), pluralize(len(r.errors), "error")))
		for _, e := range r.errors {
			lines = append(lines, fmt.Sprintf("[red]  %s[white]", e))
		}
	}
	logBufMu.Lock()
	logBuf = append(logBuf, lines...)
	logBufMu.Unlock()
}

func pluralize(n int, singular string) string {
	if n == 1 {
		return singular
	}
	return singular + "s"
}
