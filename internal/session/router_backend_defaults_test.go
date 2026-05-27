package session

import (
	"slices"
	"testing"
)

// TestBackendDefaultsFor_PrecedenceAndFallback pins the merge logic that the
// helper centralised for R222-ARCH-14 (#739): per-backend overrides win over
// router-level defaults, and a backend with no entry falls back cleanly.
func TestBackendDefaultsFor_PrecedenceAndFallback(t *testing.T) {
	t.Run("falls back to router defaults when no backend entry", func(t *testing.T) {
		r := &Router{
			model:            "router-default",
			extraArgs:        []string{"--router-flag"},
			backendModels:    map[string]string{},
			backendExtraArgs: map[string][]string{},
		}
		gotModel, gotArgs := r.backendDefaultsFor("kiro")
		if gotModel != "router-default" {
			t.Errorf("model = %q, want router default", gotModel)
		}
		if !slices.Equal(gotArgs, []string{"--router-flag"}) {
			t.Errorf("args = %v, want router default", gotArgs)
		}
	})

	t.Run("per-backend override wins over router default", func(t *testing.T) {
		r := &Router{
			model:     "router-default",
			extraArgs: []string{"--router-flag"},
			backendModels: map[string]string{
				"kiro": "kiro-model",
			},
			backendExtraArgs: map[string][]string{
				"kiro": {"--kiro-flag"},
			},
		}
		gotModel, gotArgs := r.backendDefaultsFor("kiro")
		if gotModel != "kiro-model" {
			t.Errorf("model = %q, want kiro override", gotModel)
		}
		if !slices.Equal(gotArgs, []string{"--kiro-flag"}) {
			t.Errorf("args = %v, want kiro override", gotArgs)
		}
	})

	t.Run("empty per-backend value preserves router default", func(t *testing.T) {
		// Mirrors the pre-helper inline logic: empty model / nil-or-zero
		// extraArgs entries did NOT clear router defaults — they were
		// transparent. Documented elsewhere as `bm != ""` and `len(ba) > 0`.
		r := &Router{
			model:     "router-default",
			extraArgs: []string{"--router-flag"},
			backendModels: map[string]string{
				"kiro": "",
			},
			backendExtraArgs: map[string][]string{
				"kiro": nil,
			},
		}
		gotModel, gotArgs := r.backendDefaultsFor("kiro")
		if gotModel != "router-default" {
			t.Errorf("empty backend model collapsed to %q, want router default", gotModel)
		}
		if !slices.Equal(gotArgs, []string{"--router-flag"}) {
			t.Errorf("nil backend args collapsed to %v, want router default", gotArgs)
		}
	})

	t.Run("unknown backend ID falls through cleanly", func(t *testing.T) {
		r := &Router{
			model:     "router-default",
			extraArgs: []string{"--router-flag"},
			backendModels: map[string]string{
				"kiro": "kiro-model",
			},
		}
		gotModel, gotArgs := r.backendDefaultsFor("nonexistent")
		if gotModel != "router-default" {
			t.Errorf("model = %q, want router default for unknown backend", gotModel)
		}
		if !slices.Equal(gotArgs, []string{"--router-flag"}) {
			t.Errorf("args = %v, want router default for unknown backend", gotArgs)
		}
	})
}
