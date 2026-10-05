package dispatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestAckReactionFailure_InfoOncePerPlatform: the first failed ⏳ add on a
// platform logs at Info so a missing scope is findable at the default level;
// later failures on it log at Debug, and another platform gets its own Info.
func TestAckReactionFailure_InfoOncePerPlatform(t *testing.T) {
	t.Parallel()
	d := newTestDispatcher(&fakePlatform{})
	d.platforms = map[string]platform.Platform{
		"fake":  &ackOrderPlatform{addErr: errors.New("missing_scope")},
		"other": &ackOrderPlatform{addErr: errors.New("missing_scope")},
	}
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var levels []string
	for _, name := range []string{"fake", "fake", "other"} {
		buf.Reset()
		msg := reactorMsg("m1", "hi")
		msg.Platform = name
		if d.ackQueuedWithReaction(context.Background(), msg, lg) {
			t.Fatalf("%s: add reported landed", name)
		}
		line := buf.String()
		if !strings.Contains(line, "missing_scope") {
			t.Errorf("%s: log %q lacks the error", name, line)
		}
		levels = append(levels, strings.Fields(line)[1])
	}
	if want := []string{"level=INFO", "level=DEBUG", "level=INFO"}; !slices.Equal(levels, want) {
		t.Errorf("levels = %v, want %v", levels, want)
	}
}
