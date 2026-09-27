package session

// tuning_precedence_test.go — resolveSpawnParams's session-tuning tier
// must outrank every lower layer of both chains, and empty overrides must
// fall through unchanged. docs/rfc/dashboard-model-effort-control.md §4.3 /
// §5 优先级 row.

import "testing"

func TestResolveSpawnParams_TuningPrecedence(t *testing.T) {
	newRouterWith := func(t *testing.T) *Router {
		t.Helper()
		r := NewRouter(RouterConfig{
			MaxProcs:        3,
			Model:           "cfg-default",
			BackendRuntimes: map[string]BackendRuntime{"": {}},
		})
		t.Cleanup(func() { r.Shutdown() })
		return r
	}

	t.Run("tuning model beats opts.Model and config default", func(t *testing.T) {
		r := newRouterWith(t)
		key := "dash:direct:p1:general"
		s := newSessionWithID(key, "sess-1")
		s.SetTuningModel("tuned-model")
		var sp spawnParams
		r.ss.Update(func(tx sessTx) {
			tx.Put(key, s)
			sp = r.resolveSpawnParams(tx, key, "", AgentOpts{Model: "opts-model"})
		})
		if sp.Model != "tuned-model" {
			t.Errorf("Model = %q, want tuned-model (session tuning must be highest tier)", sp.Model)
		}
	})

	t.Run("tuning effort beats opts.Effort", func(t *testing.T) {
		r := newRouterWith(t)
		key := "dash:direct:p2:general"
		s := newSessionWithID(key, "sess-2")
		s.SetTuningEffort("low")
		var sp spawnParams
		r.ss.Update(func(tx sessTx) {
			tx.Put(key, s)
			sp = r.resolveSpawnParams(tx, key, "", AgentOpts{Effort: "max"})
		})
		if sp.Effort != "low" {
			t.Errorf("Effort = %q, want low (session tuning must be highest tier)", sp.Effort)
		}
	})

	t.Run("empty tuning falls through to opts then config", func(t *testing.T) {
		r := newRouterWith(t)
		key := "dash:direct:p3:general"
		s := newSessionWithID(key, "sess-3")
		var sp spawnParams
		r.ss.Update(func(tx sessTx) {
			tx.Put(key, s)
			sp = r.resolveSpawnParams(tx, key, "", AgentOpts{Model: "opts-model", Effort: "high"})
		})
		if sp.Model != "opts-model" {
			t.Errorf("Model = %q, want opts-model (empty tuning must not mask lower tiers)", sp.Model)
		}
		if sp.Effort != "high" {
			t.Errorf("Effort = %q, want high", sp.Effort)
		}

		r.ss.Update(func(tx sessTx) {
			sp = r.resolveSpawnParams(tx, key, "", AgentOpts{})
		})
		if sp.Model != "cfg-default" {
			t.Errorf("Model = %q, want cfg-default (config fallback)", sp.Model)
		}
	})

	t.Run("fresh key without session entry has no override", func(t *testing.T) {
		r := newRouterWith(t)
		var sp spawnParams
		r.ss.Update(func(tx sessTx) {
			sp = r.resolveSpawnParams(tx, "dash:direct:new:general", "", AgentOpts{})
		})
		if sp.Model != "cfg-default" || sp.Effort != "" {
			t.Errorf("fresh key: Model=%q Effort=%q, want cfg-default/\"\"", sp.Model, sp.Effort)
		}
	})
}
