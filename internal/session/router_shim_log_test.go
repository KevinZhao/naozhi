package session

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestReconnectShims_SteadyTickLogsDiscoveryAtDebug: the reconcile loop runs
// ReconnectShimsCtx every tick, so "shim discovery complete" is INFO only when
// a manager's live set changed. The startup pass still logs it at INFO even
// though NewRouter's shimManagedKeys read the state directory first. Not
// parallel: slog.SetDefault is process-global.
func TestReconnectShims_SteadyTickLogsDiscoveryAtDebug(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness")
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
	w.ShimManager = mgr
	r := NewRouter(RouterConfig{Wrapper: w})
	t.Cleanup(r.Shutdown)

	levelOf := func(step string) string {
		t.Helper()
		buf.Reset()
		r.ReconnectShimsCtx(context.Background())
		for line := range strings.SplitSeq(buf.String(), "\n") {
			if !strings.Contains(line, `msg="shim discovery complete"`) {
				continue
			}
			for _, lvl := range []string{"INFO", "DEBUG"} {
				if strings.Contains(line, "level="+lvl+" ") {
					return lvl
				}
			}
		}
		t.Fatalf("%s: no shim discovery line in %q", step, buf.String())
		return ""
	}

	r.backends.shimManagedKeys()
	if got := levelOf("startup"); got != "INFO" {
		t.Errorf("startup pass logged discovery at %s, want INFO", got)
	}
	for _, step := range []string{"tick 1", "tick 2"} {
		if got := levelOf(step); got != "DEBUG" {
			t.Errorf("%s with an unchanged live set logged discovery at %s, want DEBUG", step, got)
		}
	}
}
