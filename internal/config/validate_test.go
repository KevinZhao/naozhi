package config

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// withRegisteredBackends temporarily clears and re-registers the backend
// profile registry around fn. Tests need a deterministic registry state
// regardless of whether other tests in this package or its dependencies
// already called RegisterDefaults. Without this guard the second
// Register("claude") would panic via the duplicate-registration check.
func withRegisteredBackends(t *testing.T, fn func()) {
	t.Helper()
	// We cannot inspect the registry directly from outside the backend
	// package, but RegisterDefaults panics on duplicate registration, so
	// recover and treat "duplicate" as "already registered, just run".
	// This keeps validate_test self-contained and independent from how
	// other test files initialize their registry.
	defer func() {
		if r := recover(); r != nil {
			// Already registered (concurrent test or prior run) — registry
			// state is fine for our purposes; just run fn.
			fn()
		}
	}()
	backend.RegisterDefaults()
	fn()
}

func TestConfig_Validate(t *testing.T) {
	withRegisteredBackends(t, func() {
		tests := []struct {
			name      string
			cfg       Config
			wantDiags int
			// wantContain is matched against each diag (Field+Msg+Hint joined)
			// for at least one diag. Empty means "no diag content check".
			wantContain []string
		}{
			{
				// Legacy single-backend (no cli.backends, no cli.backend) —
				// EnabledBackends synthesises an entry whose ID defaults to
				// "claude", which IS registered, so zero diags.
				name:      "legacy_empty_config",
				cfg:       Config{},
				wantDiags: 0,
			},
			{
				name: "legacy_explicit_claude",
				cfg: Config{CLI: CLIConfig{
					Backend: "claude",
					Path:    "/usr/local/bin/claude",
				}},
				wantDiags: 0,
			},
			{
				name: "multi_backend_all_known",
				cfg: Config{CLI: CLIConfig{
					Backends: []CLIBackendConfig{
						{ID: "claude"},
						{ID: "kiro"},
					},
				}},
				wantDiags: 0,
			},
			{
				name: "multi_backend_unknown_id",
				cfg: Config{CLI: CLIConfig{
					Backends: []CLIBackendConfig{
						{ID: "claude"},
						{ID: "gemini"}, // not registered (yet)
					},
				}},
				wantDiags: 1,
				wantContain: []string{
					"cli.backends[gemini]",
					"unknown backend id",
					// Hint must list known ids so operator can fix without docs.
					"claude",
					"kiro",
				},
			},
			{
				name: "multi_backend_two_unknowns",
				cfg: Config{CLI: CLIConfig{
					Backends: []CLIBackendConfig{
						{ID: "gemini"},
						{ID: "openai"},
					},
				}},
				wantDiags: 2,
			},
			{
				// Duplicate IDs collapse in EnabledBackends, so an unknown
				// duplicate only flags once — this guards against a
				// regression where Validate iterates raw config.Backends
				// instead of EnabledBackends() output.
				name: "duplicate_unknown_collapses",
				cfg: Config{CLI: CLIConfig{
					Backends: []CLIBackendConfig{
						{ID: "gemini"},
						{ID: "gemini"},
					},
				}},
				wantDiags: 1,
			},
			{
				// All-empty IDs path: EnabledBackends synthesises a single
				// fallback entry with the default ID ("claude"), which IS
				// registered. Validate should NOT flag the empty-ID source.
				name: "all_empty_ids_fallback",
				cfg: Config{CLI: CLIConfig{
					Path: "/usr/local/bin/claude",
					Backends: []CLIBackendConfig{
						{Path: "/usr/local/bin/claude"}, // ID omitted
					},
				}},
				wantDiags: 0,
			},
		}

		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				diags := tt.cfg.Validate()
				if len(diags) != tt.wantDiags {
					t.Fatalf("len(diags) = %d, want %d; diags = %+v",
						len(diags), tt.wantDiags, diags)
				}
				for _, expect := range tt.wantContain {
					found := false
					for _, d := range diags {
						bag := d.Field + " | " + d.Msg + " | " + d.Hint
						if strings.Contains(bag, expect) {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("no diag mentions %q; diags = %+v", expect, diags)
					}
				}
				// Every diag must carry a non-empty Field+Msg or the
				// startup log line is useless.
				for _, d := range diags {
					if d.Field == "" || d.Msg == "" {
						t.Errorf("diag missing required fields: %+v", d)
					}
					if d.Level != "warn" && d.Level != "error" {
						t.Errorf("diag.Level = %q, want warn|error", d.Level)
					}
				}
			})
		}
	})
}

// TestKnownBackendIDs_Sorted pins that hint output is alphabetical. The
// surface-level test covers determinism without making testers grep two
// hint strings to verify ordering.
func TestKnownBackendIDs_Sorted(t *testing.T) {
	withRegisteredBackends(t, func() {
		ids := knownBackendIDs()
		if len(ids) == 0 {
			t.Fatal("knownBackendIDs returned nothing; backend registry empty?")
		}
		for i := 1; i < len(ids); i++ {
			if ids[i-1] > ids[i] {
				t.Errorf("knownBackendIDs not sorted at index %d: %v", i, ids)
				break
			}
		}
		// Sanity: built-in profiles are present.
		joined := strings.Join(ids, ",")
		for _, want := range []string{"claude", "kiro"} {
			if !strings.Contains(joined, want) {
				t.Errorf("knownBackendIDs missing %q; got %v", want, ids)
			}
		}
	})
}
