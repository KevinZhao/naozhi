// Package spawndiag makes gate rejections on the spawn pipeline observable.
// Every layer that silently strips or ignores operator configuration (argv
// denylist, argv validator, env filter, capability gate, deprecated config
// fields) reports what it did as a Diag instead of a one-off slog.Warn, so the
// rejection reaches metrics, the session snapshot (/api/sessions spawn_diags)
// and the dashboard — a flag being stripped for months with only a log line as
// evidence (#2412, #2493) is the failure mode this exists to end.
//
// It sits below internal/cli on purpose: the env filter (internal/envpolicy) is
// a dependency OF cli, so it cannot report through cli without an import cycle.
// cli re-exports Diag/Emit/Observe under their historical names.
package spawndiag

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/metrics"
)

// Diag is one gate decision that altered or ignored configured input.
type Diag struct {
	// Layer names the gate: "argv-denylist" | "argv-validator" | "env-filter" |
	// "caps" | "config-deprecated" | "config-unknown".
	Layer string `json:"layer"`
	// Key is the configured thing that did not take effect ("--effort",
	// "session.workspace", "AWS_PROFILE").
	Key string `json:"key"`
	// Action is what the gate did: "dropped" | "ignored" | "rewritten".
	Action string `json:"action"`
	// Reason is one human-readable sentence.
	Reason string `json:"reason"`
}

// seen dedups emissions per scope+layer+key for the process lifetime: the
// first occurrence logs at Warn, repeats (the 30s shim reconcile heartbeat
// re-deriving argv, respawns of the same session, an env filter that runs on
// every spawn) log at Debug so the signal is not drowned by its own
// repetition. Metrics count first occurrences only, so the counter reads
// "distinct ineffective configs observed", not "heartbeat ticks".
var seen sync.Map

// observer, when non-nil, receives every emitted diag before the dedup/log
// step (so an observer sees repeats too). `naozhi config check` installs one to
// collect the diags config.Load emits.
var observer atomic.Pointer[func(scope string, d Diag)]

// Observe installs fn as the process-wide diag observer and returns a restore
// func. Single observer by design — the only consumer is the one-shot config
// check command.
func Observe(fn func(scope string, d Diag)) (restore func()) {
	observer.Store(&fn)
	return func() { observer.Store(nil) }
}

// Emit logs and counts diags. scope groups the dedup — the session key on
// spawn paths, "config" for load-time diags. The "config" scope skips dedup
// entirely: config loading is one-shot per process, and every finding there
// deserves its Warn (repeats only happen when a test re-runs the loader).
func Emit(scope string, diags []Diag) {
	for _, d := range diags {
		if obs := observer.Load(); obs != nil {
			(*obs)(scope, d)
		}
		repeat := false
		if scope != "config" {
			dedupKey := scope + "\x00" + d.Layer + "\x00" + d.Key
			_, repeat = seen.LoadOrStore(dedupKey, struct{}{})
		}
		if !repeat {
			metrics.RecordSpawnDiag(d.Layer, d.Action)
			slog.Warn("spawn gate: configured input had no effect",
				"layer", d.Layer, "key", d.Key, "action", d.Action, "reason", d.Reason, "scope", scope)
			continue
		}
		slog.Debug("spawn gate: configured input had no effect (repeat)",
			"layer", d.Layer, "key", d.Key, "action", d.Action, "scope", scope)
	}
}

// One emits a single diag; the one-line form the env filter and config
// validators use at their rejection sites.
func One(scope, layer, key, action, reason string) {
	Emit(scope, []Diag{{Layer: layer, Key: key, Action: action, Reason: reason}})
}
