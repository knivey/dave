package main

import (
	"fmt"
	"strconv"
)

// defaultTemperature is the run-index.csv label for a sweep run with the
// temperature axis unset (0 — the server-default sampling both the harness
// and prod use when the field is omitted).
const defaultTemperature = "default"

// validateTemperature enforces the -temperature axis at startup: 0 means
// "omit the field entirely" (dave's own convention sends temperature only
// when > 0) and negative values are always a mistake.
func validateTemperature(t float64) error {
	if t < 0 {
		return fmt.Errorf("-temperature must be >= 0 (0 omits the field and samples at the server default), got %v", t)
	}
	return nil
}

// temperatureValue renders the numeric axis value for the output path
// suffix: shortest round-trip form ("0.7", "1.5", "1"), deterministic for
// a given flag value.
func temperatureValue(t float64) string {
	return strconv.FormatFloat(t, 'g', -1, 64)
}

// temperatureLabel maps the axis to its run-index.csv value: "default"
// when unset, the same numeric string the path suffix uses when set.
func temperatureLabel(t float64) string {
	if t <= 0 {
		return defaultTemperature
	}
	return temperatureValue(t)
}
