package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// ValidationDiag is a single non-fatal validation finding produced by
// Config.Validate(). Diags carry enough context for the operator to locate
// and fix the offending field without re-deriving it from a free-form
// error message.
//
// Why a slice-of-diags rather than a single error: the multi-backend RFC
// (§11.1, decision-point §14.3) deliberately prefers "warn-and-continue"
// boot semantics. A typo in cli.backends should not crash the whole
// process — naozhi already silently skips unknown IDs at runtime — but it
// MUST be visible in the startup log so the operator sees it within the
// first 30s of `journalctl -u naozhi`. Returning []ValidationDiag lets the
// caller (main.go) decide the surfacing policy (slog.Warn vs slog.Error)
// and lets test code assert on individual finding fields rather than
// regex-matching a sentinel error string.
type ValidationDiag struct {
	// Level is "warn" or "error". Today every diag is "error" (unknown
	// backend ID is the only check) but the field is reserved so future
	// checks (e.g. "deprecated field still set") can warn without
	// introducing a second slice.
	Level string
	// Field is the dotted YAML path to the offending key, e.g.
	// "cli.backends[gemini]". Operators paste this into their config to
	// jump to the right line.
	Field string
	// Msg is a short human description of what's wrong. Should not
	// repeat the field name — the caller renders Field separately.
	Msg string
	// Hint is an optional remediation pointer (a list of valid values,
	// a docs link). Empty when no actionable suggestion exists.
	Hint string
}

// Validate inspects the loaded config for non-fatal mistakes that the
// regular validateConfig() pass cannot reject (because they should not
// block startup) but that the operator still needs to see.
//
// Currently checks:
//
//   - cli.backends entries whose ID is not in the backend.Profile
//     registry. main.go already silently skips unknown backends; this
//     Validate path makes the skip visible.
//
// IMPORTANT: Validate consults the global backend.registry, so it MUST
// be called after backend.RegisterDefaults(). Calling it earlier would
// flag every configured backend as unknown.
//
// Validate never returns an error — every finding goes into the slice.
// Callers decide whether to abort, warn, or ignore. multi-backend RFC
// §11.1 mandates "warn but do not block startup" so main.go currently
// just logs each diag at slog.Warn / slog.Error.
func (c *Config) Validate() []ValidationDiag {
	var diags []ValidationDiag

	backends := c.EnabledBackends()
	for _, b := range backends {
		if b.ID == "" {
			// Empty ID arises only from the legacy single-backend
			// fallback path (Config{CLI: CLIConfig{Path: "..."}}) where
			// EnabledBackends synthesises a placeholder entry. Treat
			// silently — the wrapper init step in main.go reuses
			// backend.Get("claude") for these and the operator never
			// typed an ID at all.
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

// knownBackendIDs returns every backend ID currently registered, sorted
// alphabetically so error hints are deterministic across runs (the
// registry preserves registration order, but for an unknown-ID error
// message stable output beats insertion order).
//
// Returns nil when nothing is registered — the caller should then emit
// a hint like "(no backends registered; ensure backend.RegisterDefaults
// runs before Validate)" rather than an empty list. We surface the
// empty hint case with a sentinel string so a stray Validate() before
// RegisterDefaults shows up in logs as a programmer error.
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
