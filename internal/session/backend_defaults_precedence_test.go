package session

import (
	"reflect"
	"testing"
)

// MergeBackendDefaults is the one precedence rule for a backend's spawn
// defaults, and it exists because two paths were computing it differently:
// the live spawn (Router.backendDefaultsFor, reading the router's maps) and the
// offline `naozhi shim` drift view (cmd/naozhi, reading config). The CLI took the
// per-backend value with NO fallback to the router-level base, so a backend
// inheriting the global cli.args reported a spurious DRIFT extra_args and told
// the operator to restart a healthy session (#2668).
//
// That is the third time this shape has bitten: #739 (surviving kiro shims read
// as arg-drift on every restart) and #2427 (driftCompareArgs missing DebugFile →
// restart killed live shims) were the same "two derivations of one fact" bug.

// TestMergeBackendDefaults_RouterLevelIsTheBase pins the precedence config.go
// documents: cli.backends[].model "overrides cli.model for this backend", so an
// absent per-backend value must fall back rather than blank the field.
func TestMergeBackendDefaults_RouterLevelIsTheBase(t *testing.T) {
	cases := []struct {
		name          string
		routerModel   string
		routerArgs    []string
		backendModel  string
		backendArgs   []string
		backendEffort string
		wantModel     string
		wantArgs      []string
		wantEffort    string
	}{
		{
			name:        "backend_inherits_both",
			routerModel: "opus", routerArgs: []string{"--foo"},
			wantModel: "opus", wantArgs: []string{"--foo"},
		},
		{
			name:        "backend_overrides_model_only",
			routerModel: "opus", routerArgs: []string{"--foo"},
			backendModel: "sonnet",
			wantModel:    "sonnet", wantArgs: []string{"--foo"},
		},
		{
			name:        "backend_overrides_args_only",
			routerModel: "opus", routerArgs: []string{"--foo"},
			backendArgs: []string{"--bar"},
			wantModel:   "opus", wantArgs: []string{"--bar"},
		},
		{
			name:        "backend_overrides_both",
			routerModel: "opus", routerArgs: []string{"--foo"},
			backendModel: "sonnet", backendArgs: []string{"--bar"},
			wantModel: "sonnet", wantArgs: []string{"--bar"},
		},
		{
			// Effort has no router-level tier (docs/rfc/kiro-effort-control.md §4.2).
			name:          "effort_has_no_base",
			backendEffort: "high",
			wantEffort:    "high",
		},
		{
			// An empty per-backend args slice is "unset", not "explicitly empty":
			// len(backendArgs) > 0 is the documented test, so [] must inherit.
			name:        "empty_backend_args_inherits",
			routerArgs:  []string{"--foo"},
			backendArgs: []string{},
			wantArgs:    []string{"--foo"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MergeBackendDefaults(c.routerModel, c.routerArgs, c.backendModel, c.backendArgs, c.backendEffort)
			if got.Model != c.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, c.wantModel)
			}
			if !reflect.DeepEqual(got.Args, c.wantArgs) && !(len(got.Args) == 0 && len(c.wantArgs) == 0) {
				t.Errorf("Args = %v, want %v", got.Args, c.wantArgs)
			}
			if got.Effort != c.wantEffort {
				t.Errorf("Effort = %q, want %q", got.Effort, c.wantEffort)
			}
		})
	}
}

// TestBackendDefaultsFor_MatchesMergeBackendDefaults is the parity assertion: the
// Router's map-reading path must produce exactly what the pure function does for
// the same inputs. If backendDefaultsFor ever grows its own precedence again, the
// offline drift view silently disagrees with the live spawn — which is the bug.
func TestBackendDefaultsFor_MatchesMergeBackendDefaults(t *testing.T) {
	const backendID = "kiro"
	cases := []struct {
		name         string
		routerModel  string
		routerArgs   []string
		backendModel string
		backendArgs  []string
		effort       string
	}{
		{"inherits_router_base", "opus", []string{"--foo"}, "", nil, ""},
		{"per_backend_wins", "opus", []string{"--foo"}, "sonnet", []string{"--bar"}, "high"},
		{"no_router_base", "", nil, "sonnet", []string{"--bar"}, ""},
		{"nothing_configured", "", nil, "", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Router{}
			r.bkStore.model = c.routerModel
			r.bkStore.extraArgs = c.routerArgs
			if c.backendModel != "" {
				r.bkStore.setBackendModelsForTest(map[string]string{backendID: c.backendModel})
			}
			if len(c.backendArgs) > 0 {
				r.bkStore.setBackendExtraArgsForTest(map[string][]string{backendID: c.backendArgs})
			}
			if c.effort != "" {
				r.bkStore.setBackendEffortsForTest(map[string]string{backendID: c.effort})
			}

			viaRouter := r.backendDefaultsFor(backendID)
			viaPure := MergeBackendDefaults(c.routerModel, c.routerArgs, c.backendModel, c.backendArgs, c.effort)
			if viaRouter.Model != viaPure.Model || viaRouter.Effort != viaPure.Effort ||
				!reflect.DeepEqual(viaRouter.Args, viaPure.Args) {
				t.Errorf("Router path and pure function disagree:\n router %+v\n pure   %+v\n"+
					"the offline `naozhi shim` drift view calls the pure function; any divergence "+
					"here is a spurious DRIFT on a healthy session (#2668)", viaRouter, viaPure)
			}
		})
	}
}

// TestMergeBackendDefaults_ReproducesTheSpuriousDriftInput is the specific shape
// #2668 produced: router-level cli.args set, the backend entry carrying none.
// Reading the per-backend value alone yields no args at all, which is what made
// the stored argv look drifted.
func TestMergeBackendDefaults_ReproducesTheSpuriousDriftInput(t *testing.T) {
	const routerArg = "--append-system-prompt=global"
	got := MergeBackendDefaults("opus", []string{routerArg}, "", nil, "")
	if len(got.Args) != 1 || got.Args[0] != routerArg {
		t.Fatalf("Args = %v, want [%q]: a backend with no args of its own inherits the "+
			"router-level cli.args. Dropping them is what made `naozhi shim` report DRIFT "+
			"extra_args against a healthy session (#2668).", got.Args, routerArg)
	}
	if got.Model != "opus" {
		t.Errorf("Model = %q, want \"opus\" (inherited from cli.model)", got.Model)
	}
}
