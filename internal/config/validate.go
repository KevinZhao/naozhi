package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// ValidationDiag is a non-fatal finding from Config.Validate(). A slice of
// diags (not an error) implements the multi-backend RFC §11.1 warn-and-continue
// boot semantics: the caller picks the log level, tests assert on fields.
type ValidationDiag struct {
	// Level is "warn" or "error".
	Level string
	// Field is the dotted YAML path to the offending key, e.g. "cli.backends[gemini]".
	Field string
	// Msg describes what's wrong without repeating the field name.
	Msg string
	// Hint is an optional remediation pointer.
	Hint string
}

// Validate reports non-fatal config mistakes that must not block startup but
// the operator needs to see — currently cli.backends IDs missing from the
// backend.Profile registry. MUST be called after backend.RegisterDefaults()
// or every backend is flagged unknown.
func (c *Config) Validate() []ValidationDiag {
	var diags []ValidationDiag

	backends := c.EnabledBackends()
	for _, b := range backends {
		if b.ID == "" {
			// Single-backend fallback placeholder; main resolves it to claude.
			continue
		}
		if _, ok := backend.Get(b.ID); !ok {
			diags = append(diags, ValidationDiag{
				Level: "error",
				Field: fmt.Sprintf("cli.backends[%s]", b.ID),
				Msg:   "unknown backend id; will be skipped at runtime",
				Hint:  "valid ids: " + strings.Join(knownBackendIDs(), ", "),
			})
		}
	}

	return diags
}

// knownBackendIDs returns every registered backend ID, sorted for
// deterministic hints; a sentinel string when nothing is registered so a
// Validate() before RegisterDefaults is visible in logs as a programmer error.
func knownBackendIDs() []string {
	all := backend.All()
	if len(all) == 0 {
		return []string{"(none registered)"}
	}
	ids := make([]string, len(all))
	for i, p := range all {
		ids[i] = p.ID
	}
	sort.Strings(ids)
	return ids
}
