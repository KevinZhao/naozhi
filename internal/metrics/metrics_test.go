package metrics

import (
	"encoding/json"
	"expvar"
	"testing"
)

// TestCountersRegisteredUnderStableNames pins the expvar names the operator
// runbook (docs/ops/pprof.md expvar section) depends on. Renaming any of
// these breaks dashboards and grep-based alert rules.
func TestCountersRegisteredUnderStableNames(t *testing.T) {
	t.Parallel()
	want := []string{
		"naozhi_session_create_total",
		"naozhi_session_evict_total",
		"naozhi_cli_spawn_total",
		"naozhi_ws_auth_fail_total",
		"naozhi_shim_restart_total",
	}
	for _, name := range want {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v := expvar.Get(name)
			if v == nil {
				t.Fatalf("counter %q not registered with expvar", name)
			}
			if _, ok := v.(*expvar.Int); !ok {
				t.Fatalf("counter %q is %T, want *expvar.Int", name, v)
			}
		})
	}
}

// TestCountersIncrement pins that Add(1) actually increments — the symbols
// are tiny wrappers but a future refactor that replaces expvar.Int with a
// custom type must keep the observable behaviour.
func TestCountersIncrement(t *testing.T) {
	// Not t.Parallel: counters are process-wide and other tests in the
	// binary may mutate them. We capture start values and assert the
	// delta only, which is safe for concurrent readers.
	counters := map[string]*expvar.Int{
		"session_create": SessionCreateTotal,
		"session_evict":  SessionEvictTotal,
		"cli_spawn":      CLISpawnTotal,
		"ws_auth_fail":   WSAuthFailTotal,
		"shim_restart":   ShimRestartTotal,
	}
	for name, c := range counters {
		name, c := name, c
		t.Run(name, func(t *testing.T) {
			start := c.Value()
			c.Add(1)
			c.Add(2)
			if got := c.Value() - start; got != 3 {
				t.Errorf("%s: delta %d, want 3", name, got)
			}
		})
	}
}

// TestCountersJSONEncodable pins that every counter marshals as a JSON
// number, not a string or object. expvar's /debug/vars endpoint emits each
// counter via its MarshalJSON, and operators' jq scripts assume numeric
// output.
func TestCountersJSONEncodable(t *testing.T) {
	t.Parallel()
	for _, c := range []*expvar.Int{
		SessionCreateTotal, SessionEvictTotal, CLISpawnTotal,
		WSAuthFailTotal, ShimRestartTotal,
	} {
		raw := c.String() // expvar.Int.String returns its JSON form
		var n json.Number
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			t.Fatalf("counter %p JSON %q not a number: %v", c, raw, err)
		}
	}
}
