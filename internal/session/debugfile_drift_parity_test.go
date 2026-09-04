package session

// debugfile_drift_parity_test.go — regression guard for the DebugFile arm of the
// arg-drift parity contract (see argvSpawnOptions in spawn_argv.go).
//
// Observed 2026-09-02 on a host with NAOZHI_CLI_DEBUG=1 in its launchd plist:
// every naozhi restart logged
//
//	shim config drifted, shutting down old shim
//	  old_args_len=14 new_args_len=12
//	  first_diff_old="--debug-file" first_diff_new="(absent)"
//
// and killed the surviving shim's CLI (SIGKILL via cliProc.kill → shim log
// "CLI exited code=-1"). The spawn path passed SpawnOptions.DebugFile;
// driftCompareArgs did not, so the reconstruction was permanently 2 tokens
// short and EVERY restart read EVERY live claude session as drift. A session
// that restarted naozhi from inside its own Bash tool call therefore killed
// itself, and the dashboard could not distinguish that from a long-running
// command.
//
// tuning_drift_parity_test.go did not catch it: it exercises the kiro/ACP
// backend, whose BuildArgs ignores DebugFile, and its spawn-side argv is a
// hand-written SpawnOptions literal that omitted DebugFile too — both sides
// agreed by being equally wrong, exactly the mutation-testing trap that file's
// own comment warns about.

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/shim"
)

// mkClaudeDriftRouter builds a router whose default backend is claude — the
// only protocol whose BuildArgs acts on DebugFile — with CLI debug capture on.
func mkClaudeDriftRouter(t *testing.T, debugDir string) *Router {
	t.Helper()
	r := &Router{
		ss:         sessionStore{sessions: make(map[string]*ManagedSession)},
		defaultCWD: "/default/ws",
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{
		"claude": cli.NewWrapperLazy("/bin/false", &cli.ClaudeProtocol{}, "claude"),
	}
	r.bkStore.defaultBackend = "claude"
	r.bkStore.backendOverrides = make(map[string]string)
	r.bkStore.backendEfforts = make(map[string]string)
	r.bkStore.model = "claude-sonnet-5"
	r.claudeDir = t.TempDir()
	r.cliDebugDir = debugDir
	return r
}

// TestDebugFileDriftParity_NoFalseDrift is the load-bearing assertion: with CLI
// debug capture enabled, the drift-side reconstruction must carry the same
// --debug-file token pair the real spawn emits. Before the argvSpawnOptions
// refactor this failed with drift missing "--debug-file" and its path.
func TestDebugFileDriftParity_NoFalseDrift(t *testing.T) {
	debugDir := t.TempDir()
	r := mkClaudeDriftRouter(t, debugDir)
	key := "dashboard:direct:2026-09-02-151927-3-naozhi:general"
	s := newSessionWithID(key, "sess-debugfile-1")
	s.SetBackend("claude")
	r.ss.sessions[key] = s

	// Spawn-side argv, assembled the way spawnSession does (router_lifecycle.go:
	// argvSpawnOptions + the side-effecting cliDebugFileFor).
	wrapper, backendID := r.wrapperFor("claude")
	bd := r.backendDefaultsFor(backendID)
	realArgs := wrapper.Protocol.BuildArgs(
		r.argvSpawnOptions(bd.Model, bd.Effort, r.cliDebugFileFor(key), "", bd.Args))

	// Drift-side reconstruction for the surviving shim of the same session.
	driftArgs := r.driftCompareArgs(wrapper, backendID, key, s, &shim.SpawnOverlay{})

	if !slices.Equal(realArgs, driftArgs) {
		t.Fatalf("drift reconstruction diverges from real spawn — every naozhi "+
			"restart would kill every live claude session's CLI.\n"+
			"  real:  %v\n  drift: %v", realArgs, driftArgs)
	}

	// Sanity: --debug-file actually reached argv, so the parity above is not
	// vacuous (both sides agreeing on an argv that simply lacks the flag is the
	// precise way the kiro parity test missed this bug).
	wantPath := filepath.Join(debugDir, persist.KeyHash(key)+".log")
	i := slices.Index(driftArgs, "--debug-file")
	if i < 0 || i+1 >= len(driftArgs) {
		t.Fatalf("--debug-file absent from drift argv %v — parity is vacuous", driftArgs)
	}
	if driftArgs[i+1] != wantPath {
		t.Errorf("--debug-file path = %q, want %q", driftArgs[i+1], wantPath)
	}
}

// TestDebugFileDriftParity_PathHelpersAgree pins the split introduced with
// cliDebugPathFor: the read-only helper the drift path uses and the
// side-effecting one the spawn path uses must resolve to the SAME path. If they
// ever diverge, parity breaks again even though both paths route through
// argvSpawnOptions.
func TestDebugFileDriftParity_PathHelpersAgree(t *testing.T) {
	r := mkClaudeDriftRouter(t, t.TempDir())
	key := "dashboard:direct:2026-09-02-151927-3-naozhi:general"

	if got, want := r.cliDebugPathFor(key), r.cliDebugFileFor(key); got != want {
		t.Errorf("cliDebugPathFor = %q, cliDebugFileFor = %q — drift and spawn "+
			"would emit different --debug-file values", got, want)
	}
}

// TestDebugFileDriftParity_ReadOnlyWhenComparing guards the reason the path
// helper was split out at all: drift runs over every surviving shim at startup,
// including sessions that will never respawn. It must not create debug logs as
// a side effect of merely comparing argv.
func TestDebugFileDriftParity_ReadOnlyWhenComparing(t *testing.T) {
	debugDir := t.TempDir()
	r := mkClaudeDriftRouter(t, debugDir)
	key := "dashboard:direct:never-respawns:general"
	wrapper, backendID := r.wrapperFor("claude")

	_ = r.driftCompareArgs(wrapper, backendID, key, nil, nil)

	entries, err := filepath.Glob(filepath.Join(debugDir, "*"))
	if err != nil {
		t.Fatalf("glob debug dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("drift comparison created %v in the debug dir; it must stay read-only", entries)
	}
}

// TestDebugFileDriftParity_CaptureOffEmitsNoFlag covers the default deployment
// (NAOZHI_CLI_DEBUG unset → cliDebugDir ""): no --debug-file on either side, so
// hosts that never opted in keep byte-identical argv.
func TestDebugFileDriftParity_CaptureOffEmitsNoFlag(t *testing.T) {
	r := mkClaudeDriftRouter(t, "")
	key := "dashboard:direct:no-capture:general"
	wrapper, backendID := r.wrapperFor("claude")

	args := r.driftCompareArgs(wrapper, backendID, key, nil, nil)
	if slices.Contains(args, "--debug-file") {
		t.Errorf("capture disabled but argv carries --debug-file: %v", args)
	}
}
