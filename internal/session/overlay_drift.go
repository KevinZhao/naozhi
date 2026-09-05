package session

import (
	"strings"

	"github.com/naozhi/naozhi/internal/shim"
)

// OverlayFieldDrift is one argv-bearing field whose live value (the argv the
// surviving shim was spawned with) differs from what a fresh spawn under the
// CURRENT config would use (#2543). Surfaced per session on /api/sessions as
// overlay_drift; the remedy is restarting the session — a live session is
// never auto-restarted over drift.
type OverlayFieldDrift struct {
	// Field is "model" | "effort" | "append_system_prompt", or "args" when
	// the argv differs without any of the named tokens differing (codex-class
	// backends render model/effort without dedicated flags).
	Field string `json:"field"`
	// Stored is the value in the shim's recorded argv ("" = absent).
	Stored string `json:"stored"`
	// Current is the value a fresh spawn would use now ("" = absent).
	Current string `json:"current"`
}

// driftArgvFields maps the drift-visible fields to their argv flags.
var driftArgvFields = []struct{ field, flag string }{
	{"model", "--model"},
	{"effort", "--effort"},
	{"append_system_prompt", "--append-system-prompt"},
}

// argvFlagValue extracts the value of `--flag v` / `--flag=v` from args,
// last occurrence winning, "" when absent or dangling — the same rule as
// cli.effortFromArgs, so what BuildArgs renders round-trips (pinned by
// TestOverlayDriftFields_RoundTripsBuildArgs).
func argvFlagValue(args []string, flag string) string {
	val := ""
	prefix := flag + "="
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == flag:
			if i+1 < len(args) {
				val = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], prefix):
			val = strings.TrimPrefix(args[i], prefix)
		}
	}
	return val
}

// overlayDriftFields compares the shim-recorded argv with the argv a fresh
// spawn would use today and returns the per-field differences. Equal slices
// return nil; a difference none of the named fields explains collapses into
// one {Field:"args"} entry so backends without dedicated flags still report.
func overlayDriftFields(stored, current []string) []OverlayFieldDrift {
	if equalArgv(stored, current) {
		return nil
	}
	var out []OverlayFieldDrift
	for _, f := range driftArgvFields {
		s, c := argvFlagValue(stored, f.flag), argvFlagValue(current, f.flag)
		if s != c {
			out = append(out, OverlayFieldDrift{Field: f.field, Stored: s, Current: c})
		}
	}
	if len(out) == 0 {
		out = append(out, OverlayFieldDrift{Field: "args"})
	}
	return out
}

func equalArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ShimListDrift is the offline (naozhi shim list) drift view of one
// discovered shim state against the CURRENT config's defaults. It re-merges
// the recorded SpawnOverlay over the given defaults and compares against the
// argv the shim actually runs with.
//
// The dashboard tuning layer is invisible offline, so model/effort
// differences come back in advisory — printing them as DRIFT would brand
// every tuned, healthy session as drifted and invite an operator to kill it;
// /api/sessions' overlay_drift is the authoritative per-field signal.
// append_system_prompt and extra args are tuning-independent and come back
// in drift.
func ShimListDrift(defaultModel, defaultEffort string, defaultArgs []string, profileDefaultModel string, st shim.State) (advisory, drift []OverlayFieldDrift) {
	if st.SpawnOverlay == nil || len(st.CLIArgs) == 0 {
		return nil, nil
	}
	merged := mergeArgvLayers(
		backendDefaults{Model: defaultModel, Effort: defaultEffort, Args: defaultArgs},
		profileDefaultModel, *st.SpawnOverlay, "", "")
	stored := stripResumeArgs(st.CLIArgs)

	if s := argvFlagValue(stored, "--model"); s != merged.Model {
		advisory = append(advisory, OverlayFieldDrift{Field: "model", Stored: s, Current: merged.Model})
	}
	if s := argvFlagValue(stored, "--effort"); s != merged.Effort {
		advisory = append(advisory, OverlayFieldDrift{Field: "effort", Stored: s, Current: merged.Effort})
	}
	if s := argvFlagValue(stored, "--append-system-prompt"); s != merged.SystemPrompt {
		drift = append(drift, OverlayFieldDrift{Field: "append_system_prompt", Stored: s, Current: merged.SystemPrompt})
	}
	// One-directional check for config-added args: a merged token the stored
	// argv lacks means the config grew after this shim spawned. The reverse
	// (stored-only tokens) is indistinguishable from BuildArgs-owned flags.
	storedSet := make(map[string]bool, len(stored))
	for _, a := range stored {
		storedSet[a] = true
	}
	for _, a := range merged.Args {
		if !storedSet[a] {
			drift = append(drift, OverlayFieldDrift{Field: "extra_args", Stored: "", Current: a})
		}
	}
	return advisory, drift
}
