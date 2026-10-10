package config

import (
	"errors"
	"fmt"
	"io"
)

// WriteDiagnostics writes the outcome of a load or validation for an
// operator, one finding per line: "error: ..." for every problem of err,
// "warning: ..." for every warning, "ignored: ..." for module directories
// that were not read. cfg may be nil (a load that failed); err may be any
// error, which is written as one problem. `sentineld -validate` and
// `sentinelctl validate` print the same thing.
func WriteDiagnostics(w io.Writer, cfg *Config, err error) error {
	var warnings []Problem
	var ve *ValidationError
	if errors.As(err, &ve) {
		warnings = ve.Warnings
	}
	if cfg != nil {
		warnings = append(warnings, cfg.Warnings...)
	}
	var lines []string
	for _, p := range ProblemsOf(err, "", "") {
		lines = append(lines, "error: "+p.String())
	}
	for _, p := range warnings {
		lines = append(lines, "warning: "+p.String())
	}
	if cfg != nil {
		for _, dir := range cfg.IgnoredDirs {
			lines = append(lines, "ignored: "+dir+" (module disabled or not available)")
		}
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
