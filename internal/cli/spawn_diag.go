package cli

// SpawnDiag makes gate rejections on the spawn pipeline observable. Every
// layer that silently strips or ignores operator configuration (argv
// denylist, capability gate, deprecated config fields) reports what it did as
// a SpawnDiag instead of a one-off slog.Warn, so the rejection reaches
// metrics, the session snapshot (/api/sessions spawn_diags) and the
// dashboard — a flag being stripped for months with only a log line as
// evidence (#2412, #2493) is the failure mode this exists to end.

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/metrics"
)

// SpawnDiag is one gate decision that altered or ignored configured input.
type SpawnDiag struct {
	// Layer names the gate: "argv-denylist" | "caps" | "config-deprecated".
	Layer string `json:"layer"`
	// Key is the configured thing that did not take effect ("--effort",
	// "session.workspace", "effort").
	Key string `json:"key"`
	// Action is what the gate did: "dropped" | "ignored".
	Action string `json:"action"`
	// Reason is one human-readable sentence.
	Reason string `json:"reason"`
}

// spawnDiagSeen dedups emissions per scope+layer+key for the process
// lifetime: the first occurrence logs at Warn, repeats (the 30s shim
// reconcile heartbeat re-deriving argv, respawns of the same session) log at
// Debug so the signal is not drowned by its own repetition. Metrics count
// first occurrences only, so the counter reads "distinct ineffective configs
// observed", not "heartbeat ticks".
var spawnDiagSeen sync.Map

// spawnDiagObserver, when non-nil, receives every emitted diag before the
// dedup/log step (so an observer sees repeats too). `naozhi config check`
// installs one to collect the diags config.Load emits.
var spawnDiagObserver atomic.Pointer[func(scope string, d SpawnDiag)]

// ObserveSpawnDiags installs fn as the process-wide diag observer and returns
// a restore func. Single observer by design — the only consumer is the
// one-shot config check command.
func ObserveSpawnDiags(fn func(scope string, d SpawnDiag)) (restore func()) {
	spawnDiagObserver.Store(&fn)
	return func() { spawnDiagObserver.Store(nil) }
}

// EmitSpawnDiags logs and counts diags. scope groups the dedup — the session
// key on spawn paths, "config" for load-time diags. The "config" scope skips
// dedup entirely: config loading is one-shot per process, and every finding
// there deserves its Warn (repeats only happen when a test re-runs the
// loader).
func EmitSpawnDiags(scope string, diags []SpawnDiag) {
	for _, d := range diags {
		if obs := spawnDiagObserver.Load(); obs != nil {
			(*obs)(scope, d)
		}
		repeat := false
		if scope != "config" {
			dedupKey := scope + "\x00" + d.Layer + "\x00" + d.Key
			_, repeat = spawnDiagSeen.LoadOrStore(dedupKey, struct{}{})
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

// SpawnDiagsFor derives the gate decisions BuildArgs will make for opts —
// the argv-denylist strips (same predicate filterDeniedFlags applies) and
// the capability gate ignoring an effort tier the backend cannot honour.
// Pure: no logging, no metrics; callers pass the result to EmitSpawnDiags on
// real spawn paths and to the session snapshot.
func SpawnDiagsFor(opts SpawnOptions, caps Caps) []SpawnDiag {
	var diags []SpawnDiag
	if _, over := extraArgsOverCap(opts.ExtraArgs); over {
		diags = append(diags, SpawnDiag{
			Layer:  "argv-denylist",
			Key:    "args",
			Action: "dropped",
			Reason: "ExtraArgs exceeds the argv byte cap; the whole slice is dropped",
		})
		// The cap drops everything; per-flag strips below would be noise.
		return diags
	}
	seen := map[string]bool{}
	for _, a := range opts.ExtraArgs {
		if !isDeniedFlag(a) {
			continue
		}
		name := a
		if eq := strings.IndexByte(a, '='); eq > 0 {
			name = a[:eq]
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		diags = append(diags, SpawnDiag{
			Layer:  "argv-denylist",
			Key:    name,
			Action: "dropped",
			Reason: "flag is denied in ExtraArgs; wire it through its dedicated config field",
		})
	}
	if opts.Effort != "" && !caps.EffortTier {
		diags = append(diags, SpawnDiag{
			Layer:  "caps",
			Key:    "effort",
			Action: "ignored",
			Reason: "backend does not support a thinking-effort tier",
		})
	}
	return diags
}
