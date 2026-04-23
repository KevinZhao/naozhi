package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

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
	fs := flag.NewFlagSet("naozhi shim run", flag.ExitOnError)
	key := fs.String("key", "", "session key")
	socket := fs.String("socket", "", "unix socket path")
	stateFile := fs.String("state-file", "", "state file path")
	bufferSize := fs.Int("buffer-size", 10000, "ring buffer max lines")
	maxBufBytes := fs.Int64("max-buffer-bytes", 50*1024*1024, "ring buffer max bytes")
	idleTimeout := fs.Duration("idle-timeout", 4*time.Hour, "exit after no connection for this long")
	watchdogTimeout := fs.Duration("watchdog-timeout", 30*time.Minute, "disconnect no-output timeout")
	cliPath := fs.String("cli-path", "", "path to CLI binary")
	cwd := fs.String("cwd", "", "working directory for CLI")

	// Collect --cli-arg flags (repeated)
	var cliArgs cliArgSlice
	fs.Var(&cliArgs, "cli-arg", "CLI argument (repeatable)")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	if *key == "" || *socket == "" || *cliPath == "" {
		fmt.Fprintln(os.Stderr, "required: --key, --socket, --cli-path")
		os.Exit(1)
	}

	// Setup minimal logging (JSON to stderr before we close it)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg := shim.Config{
		Key:             *key,
		SocketPath:      *socket,
		StateFile:       *stateFile,
		BufferSize:      *bufferSize,
		MaxBufBytes:     *maxBufBytes,
		IdleTimeout:     *idleTimeout,
		WatchdogTimeout: *watchdogTimeout,
		CLIPath:         *cliPath,
		CLIArgs:         []string(cliArgs),
		CWD:             *cwd,
	}

	if err := shim.Run(cfg); err != nil {
		slog.Error("shim exited with error", "err", err)
		// Write error to stdout so the parent manager can read the actual
		// failure reason instead of the generic "shim exited before ready".
		errJSON, _ := json.Marshal(err.Error())
		fmt.Fprintf(os.Stdout, `{"status":"error","error":%s}`+"\n", errJSON)
		os.Exit(1)
	}
}

func runShimStop(args []string) {
	fs := flag.NewFlagSet("naozhi shim stop", flag.ExitOnError)
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
	states, err := mgr.Discover()
	if err != nil {
		fmt.Fprintf(os.Stderr, "discover shims: %v\n", err)
		os.Exit(1)
	}

	stopped := 0
	for _, state := range states {
		if !*all && state.Key != *key {
			continue
		}
		handle, err := mgr.Reconnect(context.Background(), state.Key, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connect to %s: %v\n", state.Key, err)
			// Fallback: send SIGUSR2 for immediate shutdown
			if state.ShimPID > 0 {
				syscall.Kill(state.ShimPID, syscall.SIGUSR2) //nolint:errcheck
				fmt.Fprintf(os.Stderr, "  sent SIGUSR2 to PID %d\n", state.ShimPID)
				stopped++
			}
			continue
		}
		handle.Shutdown()
		fmt.Printf("stopped shim: key=%s pid=%d\n", state.Key, state.ShimPID)
		stopped++
	}

	if stopped == 0 && *key != "" {
		fmt.Fprintf(os.Stderr, "no shim found for key: %s\n", *key)
		os.Exit(1)
	}
	fmt.Printf("%d shim(s) stopped\n", stopped)
}

func runShimList(args []string) {
	fs := flag.NewFlagSet("naozhi shim list", flag.ExitOnError)
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
	states, err := mgr.Discover()
	if err != nil {
		fmt.Fprintf(os.Stderr, "discover shims: %v\n", err)
		os.Exit(1)
	}

	if len(states) == 0 {
		fmt.Println("no active shims")
		return
	}

	fmt.Printf("%-6s %-6s %-5s %-40s %s\n", "SHIM", "CLI", "ALIVE", "KEY", "SESSION")
	for _, s := range states {
		alive := "yes"
		if !s.CLIAlive {
			alive = "no"
		}
		sid := s.SessionID
		if len(sid) > 12 {
			sid = sid[:12] + "..."
		}
		fmt.Printf("%-6d %-6d %-5s %-40s %s\n", s.ShimPID, s.CLIPID, alive, s.Key, sid)
	}
	fmt.Printf("\n%d shim(s)\n", len(states))
}

// cliArgSlice implements flag.Value for repeated --cli-arg flags.
type cliArgSlice []string

func (s *cliArgSlice) String() string { return fmt.Sprint([]string(*s)) }
func (s *cliArgSlice) Set(val string) error {
	*s = append(*s, val)
	return nil
}
