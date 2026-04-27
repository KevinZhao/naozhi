package session

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestGetOrCreate_CreatingNewSession_IsDebug locks R84's log-level demotion.
// "creating new session" previously fired at Info immediately before
// spawnSession's own "session spawned" Info row, doubling the per-spawn
// journal noise. After the demotion it is Debug — captured only when
// operators opt into verbose logging.
//
// We drive GetOrCreate through a failing-wrapper newTestRouter so spawn
// errors out quickly; the "creating new session" log fires BEFORE
// spawnSession is invoked, so the spawn failure does not mask the
// assertion. The critical property: the captured Info-level handler must
// NOT see the "creating new session" message, while a Debug-level handler
// would still observe it.
func TestGetOrCreate_CreatingNewSession_IsDebug(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)

	var info bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&info, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	r := newTestRouter(5)
	_, _, _ = r.GetOrCreate(context.Background(), "feishu:group:t:a", AgentOpts{})

	if strings.Contains(info.String(), "creating new session") {
		t.Errorf("Info-level handler observed %q — demotion to Debug broken\nbuffer=%s",
			"creating new session", info.String())
	}
}

// TestGetOrCreate_CreatingNewSession_VisibleAtDebug verifies the counter
// half of the demotion: the message is still there, just at Debug. A future
// refactor that deletes the log line entirely would silently pass the
// Info-level assertion above, so pair it with a positive-observation test.
func TestGetOrCreate_CreatingNewSession_VisibleAtDebug(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)

	var dbg bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&dbg, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	r := newTestRouter(5)
	_, _, _ = r.GetOrCreate(context.Background(), "feishu:group:t:a", AgentOpts{})

	if !strings.Contains(dbg.String(), "creating new session") {
		t.Errorf("Debug-level handler missing %q — log line accidentally deleted\nbuffer=%s",
			"creating new session", dbg.String())
	}
}
