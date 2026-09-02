package session

// tuning_drift_parity_test.go — RFC §4.5 的最高风险回归护栏:带
// TuningModel/TuningEffort 的会话,重启后的 arg-drift 重建必须与真实 spawn
// 产出相同 argv,否则每次 naozhi 重启都会把切过模型/档位的存活 kiro 会话
// 误判为漂移并重启(操作者可见为"切过模型的会话一重启全丢")。
// docs/rfc/dashboard-model-effort-control.md §4.5 / §5 arg-drift row.

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func mkTuningRouter(t *testing.T) *Router {
	t.Helper()
	r := &Router{
		ss:         sessionStore{sessions: make(map[string]*ManagedSession)},
		defaultCWD: "/default/ws",
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{
		"kiro": cli.NewWrapper("/bin/false", &cli.ACPProtocol{BackendID: "kiro"}, "kiro"),
	}
	r.bkStore.defaultBackend = "kiro"
	r.bkStore.backendOverrides = make(map[string]string)
	r.bkStore.backendEfforts = map[string]string{"kiro": "high"}
	r.bkStore.model = "claude-fable-5"
	r.wsStore.overrides = make(map[string]string)
	r.claudeDir = t.TempDir()
	r.kiroSessionsDir = t.TempDir()
	return r
}

// TestTuningDriftParity_NoFalseDrift is the load-bearing assertion: for a
// session carrying tuning overrides, the drift-side argv reconstruction
// (driftCompareArgs — what classifyShimState compares stored shim argv
// against) must equal the real-spawn argv (resolveSpawnParamsLocked →
// BuildArgs). Both sides run the PRODUCTION code paths — building either
// argv by hand here would repeat the mutation-testing trap documented in
// effort_drift_parity_test.go (a test that constructs its own SpawnOptions
// passes even after the production literal loses a field).
func TestTuningDriftParity_NoFalseDrift(t *testing.T) {
	r := mkTuningRouter(t)
	key := "dash:direct:drift:general"
	s := newSessionWithID(key, "sess-drift-1")
	s.SetBackend("kiro")
	s.SetTuningModel("claude-haiku-4.5")
	s.SetTuningEffort("low")
	r.ss.sessions[key] = s

	// Real spawn argv, exactly as spawnSession assembles it.
	sp := r.resolveSpawnParamsLocked(key, "", AgentOpts{Backend: "kiro", Workspace: "/ws"})
	realArgs := sp.Wrapper.Protocol.BuildArgs(cli.SpawnOptions{
		Model:        sp.Model,
		ExtraArgs:    sp.Args,
		Effort:       sp.Effort,
		SettingsFile: r.naozhiSettingsFile,
	})

	// Drift-side reconstruction for the surviving shim of the same session.
	wrapper, backendID := r.wrapperFor("kiro")
	driftArgs := r.driftCompareArgs(wrapper, backendID, s)

	if !slices.Equal(realArgs, driftArgs) {
		t.Fatalf("drift reconstruction diverges from real spawn — every naozhi "+
			"restart would misclassify this tuned session as arg-drift and restart it.\n"+
			"  real:  %v\n  drift: %v", realArgs, driftArgs)
	}
	// Sanity: the overrides actually reached argv (guards against both sides
	// agreeing because both silently dropped the tuning tier).
	if !slices.Contains(realArgs, "claude-haiku-4.5") || !slices.Contains(realArgs, "low") {
		t.Fatalf("tuning values missing from argv %v — parity above is vacuous", realArgs)
	}
}

// TestTuningDriftParity_ChangedOverrideIsRealDrift pins the intended
// semantics of §4.5's "语义确认": when the operator changes the override
// while the process is alive and naozhi restarts, the drift IS genuine —
// the reconstruction reflects the NEW override, diverges from the stored
// argv, and the resulting restart is what applies the new value.
func TestTuningDriftParity_ChangedOverrideIsRealDrift(t *testing.T) {
	r := mkTuningRouter(t)
	key := "dash:direct:drift2:general"
	s := newSessionWithID(key, "sess-drift-2")
	s.SetBackend("kiro")
	s.SetTuningModel("claude-haiku-4.5")
	r.ss.sessions[key] = s

	wrapper, backendID := r.wrapperFor("kiro")
	storedArgs := r.driftCompareArgs(wrapper, backendID, s) // argv the shim recorded at spawn

	s.SetTuningModel("claude-sonnet-4.6") // operator switches model, then naozhi restarts
	newArgs := r.driftCompareArgs(wrapper, backendID, s)

	if slices.Equal(storedArgs, newArgs) {
		t.Fatal("changed override did not surface as drift — the respawn that " +
			"applies the new model would never trigger")
	}
}

// TestTuningDriftParity_NilSessionFallsBack covers the adopt path: a shim
// whose key has no ManagedSession yet (crash before first store save) has no
// tuning by construction; the reconstruction must degrade to backend
// defaults, not panic.
func TestTuningDriftParity_NilSessionFallsBack(t *testing.T) {
	r := mkTuningRouter(t)
	wrapper, backendID := r.wrapperFor("kiro")
	args := r.driftCompareArgs(wrapper, backendID, nil)
	if !slices.Contains(args, "claude-fable-5") || !slices.Contains(args, "high") {
		t.Errorf("nil-session drift args must carry backend defaults, got %v", args)
	}
}

// TestTuningDriftParity_SurvivesRespawn extends the parity guard past the
// first incarnation: after a tuning respawn replaces the ManagedSession
// (installFreshSessionLocked), the NEW entry must still reconstruct the same
// argv the spawn actually used. Otherwise the very restart that follows a
// model switch misreads the tuned shim as arg-drift and rebuilds it default.
func TestTuningDriftParity_SurvivesRespawn(t *testing.T) {
	r := mkTuningRouter(t)
	// installFreshSessionLocked touches the id indexes that NewRouter
	// normally allocates; the minimal fixture above does not.
	r.ss.idToKey = map[string]string{}
	r.kid.ids = map[string]bool{}
	key := "dash:direct:drift3:general"
	s := newSessionWithID(key, "sess-drift-3")
	s.SetBackend("kiro")
	s.SetTuningModel("claude-haiku-4.5")
	s.SetTuningEffort("low")
	r.ss.sessions[key] = s

	// argv of the respawn, as spawnSession assembles it from the OLD entry.
	sp := r.resolveSpawnParamsLocked(key, "sess-drift-3", AgentOpts{Backend: "kiro", Workspace: "/ws"})
	realArgs := sp.Wrapper.Protocol.BuildArgs(cli.SpawnOptions{
		Model:        sp.Model,
		ExtraArgs:    sp.Args,
		Effort:       sp.Effort,
		SettingsFile: r.naozhiSettingsFile,
	})

	// The spawn then replaces the entry, carrying the snapshotted overrides.
	_, _, _, _, ov := snapshotOldSessionLocked(s)
	fresh := r.installFreshSessionLocked(
		key, &cli.Process{}, "/ws", "kiro", "", sp.Wrapper, "sess-drift-3",
		nil, nil, 0, 0, 0, false, "sess-drift-3", 0, ov,
	)

	wrapper, backendID := r.wrapperFor("kiro")
	driftArgs := r.driftCompareArgs(wrapper, backendID, fresh)
	if !slices.Equal(realArgs, driftArgs) {
		t.Fatalf("post-respawn entry diverges from the argv it was spawned with — "+
			"the next naozhi restart would rebuild this tuned session as default.\n"+
			"  real:  %v\n  drift: %v", realArgs, driftArgs)
	}
	if !slices.Contains(realArgs, "claude-haiku-4.5") || !slices.Contains(realArgs, "low") {
		t.Fatalf("tuning values missing from argv %v — parity above is vacuous", realArgs)
	}
}
