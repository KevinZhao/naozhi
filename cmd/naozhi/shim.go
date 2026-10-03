package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/shim"
)

// runShim handles the "naozhi shim" subcommand family.
func runShim(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: naozhi shim <run|stop|list>")
		os.Exit(1)
	}

	switch args[0] {
	case "run":
		runShimRun(args[1:])
	case "stop":
		runShimStop(args[1:])
	case "list":
		runShimList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown shim command: %s\n", args[0])
		os.Exit(1)
	}
}

func runShimRun(args []string) {
	fs := newFlagSet("naozhi shim run")
	key := fs.String("key", "", "session key")
	socket := fs.String("socket", "", "unix socket path")
	stateFile := fs.String("state-file", "", "state file path")
	bufferSize := fs.Int("buffer-size", 10000, "ring buffer max lines")
	maxBufBytes := fs.Int64("max-buffer-bytes", 50*1024*1024, "ring buffer max bytes")
	idleTimeout := fs.Duration("idle-timeout", 4*time.Hour, "exit after no connection for this long")
	watchdogTimeout := fs.Duration("watchdog-timeout", 30*time.Minute, "disconnect no-output timeout")
	cliPath := fs.String("cli-path", "", "path to CLI binary")
	backend := fs.String("backend", "", "backend id (claude/kiro); recorded in state file")
	cwd := fs.String("cwd", "", "working directory for CLI")
	spawnOverlayJSON := fs.String("spawn-overlay", "", "JSON per-request spawn overlay; recorded verbatim in the state file (#2494)")

	var cliArgs cliArgSlice
	fs.Var(&cliArgs, "cli-arg", "CLI argument (repeatable)")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	if *key == "" || *socket == "" || *cliPath == "" {
		fmt.Fprintln(os.Stderr, "required: --key, --socket, --cli-path")
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Written by the parent naozhi, so a decode failure is version skew.
	// Refuse to start: silently dropping it would read as "legacy, overlay
	// unknown" on the next restart (#2494).
	spawnOverlay, err := shim.DecodeSpawnOverlay(*spawnOverlayJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --spawn-overlay: %v\n", err)
		os.Exit(1)
	}

	cfg := shim.Config{
		Key:             *key,
		SocketPath:      *socket,
		StateFile:       *stateFile,
		BufferSize:      *bufferSize,
		MaxBufBytes:     *maxBufBytes,
		IdleTimeout:     *idleTimeout,
		WatchdogTimeout: *watchdogTimeout,
		CLIPath:         *cliPath,
		Backend:         *backend,
		CLIArgs:         []string(cliArgs),
		CWD:             *cwd,
		SpawnOverlay:    spawnOverlay,
	}

	if err := shim.Run(cfg); err != nil {
		slog.Error("shim exited with error", "err", err)
		// stdout is what the parent manager reads for the failure reason.
		errJSON, _ := json.Marshal(err.Error())
		fmt.Fprintf(os.Stdout, `{"status":"error","error":%s}`+"\n", errJSON)
		os.Exit(1)
	}
}

func runShimStop(args []string) {
	fs := newFlagSet("naozhi shim stop")
	key := fs.String("key", "", "session key to stop")
	all := fs.Bool("all", false, "stop all shims")
	stateDir := fs.String("state-dir", "", "shim state directory")
	fs.Parse(args) //nolint:errcheck

	if !*all && *key == "" {
		fmt.Fprintln(os.Stderr, "required: --key or --all")
		os.Exit(1)
	}

	if *stateDir == "" {
		home, _ := os.UserHomeDir()
		*stateDir = home + "/.naozhi/shims"
	}

	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: *stateDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "init shim manager: %v\n", err)
		os.Exit(1)
	}
	if code := stopShims(mgr, *key, *all, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// stopShims stops the targets planShimStop picks from a read-only Inspect, so
// non-target state files are never touched, and returns the exit code.
func stopShims(mgr *shim.Manager, key string, all bool, stdout, stderr io.Writer) int {
	entries, err := mgr.Inspect()
	if err != nil {
		fmt.Fprintf(stderr, "inspect shims: %v\n", err)
		return 1
	}
	plan := planShimStop(entries, key, all)

	stopped := 0
	for _, state := range plan.targets {
		handle, err := mgr.Reconnect(context.Background(), state.Key, 0)
		if err != nil {
			fmt.Fprintf(stderr, "connect to %s: %v\n", state.Key, err)
			if shim.SignalAfterFailedReconnect(state.ShimPID, err) {
				fmt.Fprintf(stderr, "  sent SIGUSR2 to PID %d\n", state.ShimPID)
				stopped++
			}
			continue
		}
		handle.Shutdown()
		fmt.Fprintf(stdout, "stopped shim: key=%s pid=%d\n", state.Key, state.ShimPID)
		stopped++
	}
	for _, e := range plan.skipped {
		fmt.Fprintf(stderr, "skipped %s: %s\n", e.State.Key, shimSkipReason(e))
	}
	if plan.stale > 0 {
		fmt.Fprintf(stderr, "%d stale state file(s) left for the service's reconcile to clean\n", plan.stale)
	}

	if stopped == 0 && len(plan.skipped) == 0 && key != "" {
		fmt.Fprintf(stderr, "no shim found for key: %s\n", key)
		return 1
	}
	fmt.Fprintf(stdout, "%d shim(s) stopped\n", stopped)
	if len(plan.skipped) > 0 {
		return 1
	}
	return 0
}

// shimStopPlan is what `shim stop` does with Inspect's entries. Only targets
// whose PID is alive and confirmed to run this binary are signalled; an
// unconfirmed PID may belong to an unrelated process that reused it.
type shimStopPlan struct {
	targets []shim.State
	skipped []shim.StateEntry
	stale   int
}

func planShimStop(entries []shim.StateEntry, key string, all bool) shimStopPlan {
	var p shimStopPlan
	for _, e := range entries {
		if !all && (e.Verdict == shim.StateCorrupt || e.State.Key != key) {
			continue
		}
		switch {
		case !shimEntryLive(e):
			p.stale++
		case e.Verdict == shim.StateBinaryMismatch || e.IdentityErr != nil:
			p.skipped = append(p.skipped, e)
		default:
			p.targets = append(p.targets, e.State)
		}
	}
	return p
}

func shimSkipReason(e shim.StateEntry) string {
	if e.Verdict == shim.StateBinaryMismatch {
		return fmt.Sprintf("pid %d runs a different naozhi binary than this one; rerun with the service's binary, or check the PID and kill it by hand", e.State.ShimPID)
	}
	return fmt.Sprintf("cannot confirm pid %d is a naozhi shim (%v); check the PID and kill it by hand", e.State.ShimPID, e.IdentityErr)
}

func runShimList(args []string) {
	fs, configPath := newSubFlagSet("naozhi shim list", "config.yaml")
	stateDir := fs.String("state-dir", "", "shim state directory")
	fs.Parse(args) //nolint:errcheck

	if *stateDir == "" {
		home, _ := os.UserHomeDir()
		*stateDir = home + "/.naozhi/shims"
	}

	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: *stateDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "init shim manager: %v\n", err)
		os.Exit(1)
	}
	if code := listShims(mgr, *configPath, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// listShims prints a read-only Inspect of the state dir and returns the exit code.
func listShims(mgr *shim.Manager, configPath string, stdout, stderr io.Writer) int {
	entries, err := mgr.Inspect()
	if err != nil {
		fmt.Fprintf(stderr, "inspect shims: %v\n", err)
		return 1
	}

	// Drift against current config is best-effort: an unreadable config only
	// disables the drift lines, it never breaks the listing.
	var cfg *config.Config
	if slices.ContainsFunc(entries, shimEntryLive) {
		var cfgErr error
		if cfg, cfgErr = config.Load(configPath); cfgErr != nil {
			fmt.Fprintf(stderr, "note: config unavailable (%v); overlay drift check skipped\n", cfgErr)
			cfg = nil
		}
	}
	writeShimList(stdout, entries, cfg)
	return 0
}

// shimEntryLive reports whether the state file's PID is alive.
func shimEntryLive(e shim.StateEntry) bool {
	return e.Verdict != shim.StateCorrupt && e.Verdict != shim.StateDeadPID
}

// shimListStatus is the STATUS column for a state file whose PID is alive.
func shimListStatus(e shim.StateEntry) string {
	switch {
	case e.Verdict == shim.StateBinaryMismatch:
		return "foreign-bin"
	case e.IdentityErr != nil:
		return "unverified"
	case e.Verdict == shim.StateSocketMissing:
		return "no-socket"
	}
	return "ok"
}

// writeShimList prints every state file whose PID is alive, then counts the
// dead and corrupt ones. cfg nil skips the drift lines.
func writeShimList(w io.Writer, entries []shim.StateEntry, cfg *config.Config) {
	var live []shim.StateEntry
	stale, foreign := 0, false
	for _, e := range entries {
		if !shimEntryLive(e) {
			stale++
			continue
		}
		live = append(live, e)
		foreign = foreign || e.Verdict == shim.StateBinaryMismatch
	}

	if len(live) == 0 {
		fmt.Fprintln(w, "no active shims")
	} else {
		fmt.Fprintf(w, "%-6s %-6s %-5s %-40s %-15s %s\n", "SHIM", "CLI", "ALIVE", "KEY", "SESSION", "STATUS")
		for _, e := range live {
			s := e.State
			alive := "yes"
			if !s.CLIAlive {
				alive = "no"
			}
			sid := s.SessionID
			if len(sid) > 12 {
				sid = sid[:12] + "..."
			} else if sid == "" {
				sid = "-"
			}
			fmt.Fprintf(w, "%-6d %-6d %-5s %-40s %-15s %s\n", s.ShimPID, s.CLIPID, alive, s.Key, sid, shimListStatus(e))
			if cfg != nil {
				printShimDrift(w, cfg, s)
			}
		}
		fmt.Fprintf(w, "\n%d shim(s)\n", len(live))
	}
	if foreign {
		fmt.Fprintln(w, "foreign-bin: started by a different naozhi binary than this one; run list/stop with the service's binary")
	}
	if stale > 0 {
		fmt.Fprintf(w, "%d stale state file(s) left for the service's reconcile to clean\n", stale)
	}
}

// printShimDrift prints the overlay-drift view of one shim state against the
// loaded config (#2543). append_system_prompt / extra_args differences are
// DRIFT; model/effort go out as advisory only — the dashboard tuning layer
// is invisible offline, so a hard DRIFT there would brand every tuned,
// healthy session as broken (the authoritative per-field signal is
// /api/sessions overlay_drift).
func printShimDrift(w io.Writer, cfg *config.Config, st shim.State) {
	// The router-level cli.model / cli.args are the BASE; the per-backend entry
	// only overrides them when non-empty. Reading b.Model/b.Args directly (what
	// this did before #2668) dropped the base, so a backend inheriting the global
	// cli.args produced a spurious DRIFT extra_args on a healthy session.
	// MergeBackendDefaults is the same function the live spawn path goes through.
	var bd session.BackendDefaults
	for _, b := range cfg.EnabledBackends() {
		id := b.ID
		if id == "" {
			id = "claude"
		}
		if id == st.Backend || (st.Backend == "" && id == "claude") {
			bd = session.MergeBackendDefaults(cfg.CLI.Model, cfg.CLI.Args, b.Model, b.Args, b.Effort)
			break
		}
	}
	profileModel := ""
	if st.SpawnOverlay != nil && st.SpawnOverlay.AccessProfile != "" {
		if p, ok := cfg.AccessProfiles[st.SpawnOverlay.AccessProfile]; ok {
			profileModel = p.DefaultModel
		}
	}
	advisory, drift := session.ShimListDrift(bd, profileModel, st)
	for _, d := range drift {
		fmt.Fprintf(w, "       DRIFT %s: %q -> %q — 重启会话以应用新配置\n", d.Field, d.Stored, d.Current)
	}
	for _, d := range advisory {
		fmt.Fprintf(w, "       note %s: stored %q, config now %q（不含 dashboard tuning 层，以 /api/sessions 的 overlay_drift 为准）\n", d.Field, d.Stored, d.Current)
	}
}

// cliArgSlice implements flag.Value for repeated --cli-arg flags.
type cliArgSlice []string

func (s *cliArgSlice) String() string { return fmt.Sprint([]string(*s)) }
func (s *cliArgSlice) Set(val string) error {
	*s = append(*s, val)
	return nil
}
