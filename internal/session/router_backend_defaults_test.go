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
		r := &Router{}
		r.bkStore.model = "router-default"
		r.bkStore.extraArgs = []string{"--router-flag"}
		r.bkStore.backendModels = map[string]string{}
		r.bkStore.backendExtraArgs = map[string][]string{}
		bd := r.backendDefaultsFor("kiro")
		if bd.Model != "router-default" {
			t.Errorf("model = %q, want router default", bd.Model)
		}
		if !slices.Equal(bd.Args, []string{"--router-flag"}) {
			t.Errorf("args = %v, want router default", bd.Args)
		}
	})

	t.Run("per-backend override wins over router default", func(t *testing.T) {
		r := &Router{}
		r.bkStore.model = "router-default"
		r.bkStore.extraArgs = []string{"--router-flag"}
		r.bkStore.backendModels = map[string]string{
			"kiro": "kiro-model",
		}
		r.bkStore.backendExtraArgs = map[string][]string{
			"kiro": {"--kiro-flag"},
		}
		bd := r.backendDefaultsFor("kiro")
		if bd.Model != "kiro-model" {
			t.Errorf("model = %q, want kiro override", bd.Model)
		}
		if !slices.Equal(bd.Args, []string{"--kiro-flag"}) {
			t.Errorf("args = %v, want kiro override", bd.Args)
		}
	})

	t.Run("empty per-backend value preserves router default", func(t *testing.T) {
		// Mirrors the pre-helper inline logic: empty model / nil-or-zero
		// extraArgs entries did NOT clear router defaults — they were
		// transparent. Documented elsewhere as `bm != ""` and `len(ba) > 0`.
		r := &Router{}
		r.bkStore.model = "router-default"
		r.bkStore.extraArgs = []string{"--router-flag"}
		r.bkStore.backendModels = map[string]string{
			"kiro": "",
		}
		r.bkStore.backendExtraArgs = map[string][]string{
			"kiro": nil,
		}
		bd := r.backendDefaultsFor("kiro")
		if bd.Model != "router-default" {
			t.Errorf("empty backend model collapsed to %q, want router default", bd.Model)
		}
		if !slices.Equal(bd.Args, []string{"--router-flag"}) {
			t.Errorf("nil backend args collapsed to %v, want router default", bd.Args)
		}
	})

	// Effort has no router-level base (the composition root already folded
	// cli.effort into the per-backend map and dropped it for backends whose
	// protocol ignores it), so the only question is per-backend lookup.
	// docs/rfc/kiro-effort-control.md §4.2
	t.Run("effort comes from the per-backend map only", func(t *testing.T) {
		r := &Router{}
		r.bkStore.backendEfforts = map[string]string{"kiro": "xhigh"}

		if got := r.backendDefaultsFor("kiro").Effort; got != "xhigh" {
			t.Errorf("effort = %q, want xhigh", got)
		}
		// A backend with no entry must get "" — passing a tier to a backend
		// the operator didn't configure would silently change its behaviour.
		if got := r.backendDefaultsFor("claude").Effort; got != "" {
			t.Errorf("effort for unconfigured backend = %q, want empty", got)
		}
		// Nil map (no effort configured anywhere) must not panic.
		r2 := &Router{}
		if got := r2.backendDefaultsFor("kiro").Effort; got != "" {
			t.Errorf("effort with nil map = %q, want empty", got)
		}
	})

	t.Run("unknown backend ID falls through cleanly", func(t *testing.T) {
		r := &Router{}
		r.bkStore.model = "router-default"
		r.bkStore.extraArgs = []string{"--router-flag"}
		r.bkStore.backendModels = map[string]string{
			"kiro": "kiro-model",
		}
		bd := r.backendDefaultsFor("nonexistent")
		if bd.Model != "router-default" {
			t.Errorf("model = %q, want router default for unknown backend", bd.Model)
		}
		if !slices.Equal(bd.Args, []string{"--router-flag"}) {
			t.Errorf("args = %v, want router default for unknown backend", bd.Args)
		}
	})
}
