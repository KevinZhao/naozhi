// rule 5 (stale_exemption): until_phase only says how long a file_size
// exemption is valid; nothing forces its removal once the file shrinks or
// disappears. This rule fails entries whose file does not exist so
// exemption debt cannot accumulate silently.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// scanStaleExemption returns a Violation for every file_size exemption
// entry that references a non-existent file.
func scanStaleExemption(exempts *exemptions) []Violation {
	var out []Violation
	for _, e := range exempts.FileSize {
		if _, err := os.Stat(e.Path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			continue
		}
		out = append(out, Violation{
			Rule: "stale_exemption",
			File: filepath.ToSlash(e.Path),
			Message: fmt.Sprintf("exemption entry references non-existent file %q (until_phase: %s) — entry should be removed; Phase X PR commit message must include 'Closes-exemption: %s'",
				e.Path, e.UntilPhase, e.Path),
		})
	}
	return out
}
