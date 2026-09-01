package session

// tuning_precedence_test.go — resolveSpawnParamsLocked's session-tuning tier
// must outrank every lower layer of both chains, and empty overrides must
// fall through unchanged. docs/rfc/dashboard-model-effort-control.md §4.3 /
// §5 优先级 row.

import "testing"

func TestResolveSpawnParams_TuningPrecedence(t *testing.T) {
	newRouterWith := func(t *testing.T) *Router {
		t.Helper()
		r := NewRouter(RouterConfig{
			MaxProcs:       3,
			Model:          "cfg-default",
			BackendEfforts: map[string]string{"": ""},
		})
		t.Cleanup(func() { r.Shutdown() })
		return r
	}

	t.Run("tuning model beats opts.Model and config default", func(t *testing.T) {
		r := newRouterWith(t)
		key := "dash:direct:p1:general"
		s := newSessionWithID(key, "sess-1")
		s.SetTuningModel("tuned-model")
		r.mu.Lock()
		r.ss.sessions[key] = s
		sp := r.resolveSpawnParamsLocked(key, "", AgentOpts{Model: "opts-model"})
		r.mu.Unlock()
		if sp.Model != "tuned-model" {
			t.Errorf("Model = %q, want tuned-model (session tuning must be highest tier)", sp.Model)
		}
	})

	t.Run("tuning effort beats opts.Effort", func(t *testing.T) {
		r := newRouterWith(t)
		key := "dash:direct:p2:general"
		s := newSessionWithID(key, "sess-2")
		s.SetTuningEffort("low")
		r.mu.Lock()
		r.ss.sessions[key] = s
		sp := r.resolveSpawnParamsLocked(key, "", AgentOpts{Effort: "max"})
		r.mu.Unlock()
		if sp.Effort != "low" {
			t.Errorf("Effort = %q, want low (session tuning must be highest tier)", sp.Effort)
		}
	})

	t.Run("empty tuning falls through to opts then config", func(t *testing.T) {
		r := newRouterWith(t)
		key := "dash:direct:p3:general"
		s := newSessionWithID(key, "sess-3")
		r.mu.Lock()
		r.ss.sessions[key] = s
		sp := r.resolveSpawnParamsLocked(key, "", AgentOpts{Model: "opts-model", Effort: "high"})
		r.mu.Unlock()
		if sp.Model != "opts-model" {
			t.Errorf("Model = %q, want opts-model (empty tuning must not mask lower tiers)", sp.Model)
		}
		if sp.Effort != "high" {
			t.Errorf("Effort = %q, want high", sp.Effort)
		}

		r.mu.Lock()
		sp = r.resolveSpawnParamsLocked(key, "", AgentOpts{})
		r.mu.Unlock()
		if sp.Model != "cfg-default" {
			t.Errorf("Model = %q, want cfg-default (config fallback)", sp.Model)
		}
	})

	t.Run("fresh key without session entry has no override", func(t *testing.T) {
		r := newRouterWith(t)
		r.mu.Lock()
		sp := r.resolveSpawnParamsLocked("dash:direct:new:general", "", AgentOpts{})
		r.mu.Unlock()
		if sp.Model != "cfg-default" || sp.Effort != "" {
			t.Errorf("fresh key: Model=%q Effort=%q, want cfg-default/\"\"", sp.Model, sp.Effort)
		}
	})
}
