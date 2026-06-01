package cron

import (
	"bytes"
	"log/slog"
	"regexp"
	"strconv"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestNotifyTarget_CapDropFoldedIntoFailureWarn pins the R236-SEC-15
// follow-up: when SplitText produces MORE than cronNotifyMaxChunks chunks
// (so the cap truncates a tail) AND a subsequent send fails mid-flush, the
// abort-path WARN must account for the cap-dropped tail. Before the fix the
// WARN reported `total = len(chunks)` (the post-cap subset), silently
// under-reporting how many chunks the recipient never received.
//
// Not t.Parallel: slog.SetDefault is process-global.
func TestNotifyTarget_CapDropFoldedIntoFailureWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	// Produce well more than cronNotifyMaxChunks chunks so the cap drops a
	// tail (dropped > 0). Fail on the second chunk so the loop aborts after
	// the cap truncation but before delivering every retained chunk.
	const produced = 12
	if produced <= cronNotifyMaxChunks {
		t.Fatalf("test fixture: produced=%d must exceed cap=%d", produced, cronNotifyMaxChunks)
	}
	fp := &fakePartialPlatform{failAt: 1, maxLen: 8}
	s := &Scheduler{}
	s.configMapsPtr.Store(&cronConfigMaps{
		platforms: map[string]platform.Platform{"fake-notify": fp},
	})
	long := buildDistinctChunks(produced, 8)
	chunks := platform.SplitText(long, 8)
	if len(chunks) <= cronNotifyMaxChunks {
		t.Fatalf("test fixture: SplitText produced %d chunks, want > cap %d", len(chunks), cronNotifyMaxChunks)
	}
	wantDropped := len(chunks) - cronNotifyMaxChunks
	wantTotal := cronNotifyMaxChunks + wantDropped // == len(chunks): the true original count

	s.notifyTarget("fake-notify", "chat-x", long)

	out := buf.String()
	// The failure-abort WARN must carry total=<original chunk count>, not the
	// post-cap subset (cronNotifyMaxChunks).
	re := regexp.MustCompile(`chunks dropped after send failure.*?\btotal=(\d+)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("did not find failure-abort WARN with total= field; log was:\n%s", out)
	}
	gotTotal, _ := strconv.Atoi(m[1])
	if gotTotal != wantTotal {
		t.Fatalf("WARN total=%d, want %d (must fold cap-dropped tail of %d into the count)", gotTotal, wantTotal, wantDropped)
	}
	if gotTotal == cronNotifyMaxChunks {
		t.Fatalf("WARN total=%d equals the post-cap subset; cap-dropped tail not accounted for", gotTotal)
	}
}
