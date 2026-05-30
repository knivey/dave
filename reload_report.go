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
		errNoun := "error"
		if len(r.errors) != 1 {
			errNoun = "errors"
		}
		fmt.Fprintf(view, "[yellow]── %s completed with %d %s ──[white]\n",
			r.label, len(r.errors), errNoun)
	}
	for _, s := range r.successes {
		fmt.Fprintf(view, "  %s\n", s)
	}
	if len(r.errors) > 0 {
		errNoun := "error"
		if len(r.errors) != 1 {
			errNoun = "errors"
		}
		fmt.Fprintf(view, "[red]── %d %s ──[white]\n", len(r.errors), errNoun)
		for _, e := range r.errors {
			fmt.Fprintf(view, "[red]  %s[white]\n", e)
		}
	}
}
