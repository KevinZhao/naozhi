package persist

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureHandler is a minimal slog.Handler that records every Warn-and-
// above record's formatted message + attribute keys for assertion.
// Only used in this test; constructed inline to keep the fixture small.
type captureHandler struct {
	mu      sync.Mutex
	records []string // each is "msg|key1=value1|key2=value2..."
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte('|')
		b.WriteString(a.Key)
		b.WriteByte('=')
		fmt.Fprintf(&b, "%v", a.Value.Any())
		return true
	})
	h.records = append(h.records, b.String())
	return nil
}
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.records))
	copy(out, h.records)
	return out
}

// TestPersister_FullChannel_LogsChannelUsed pins R250-ARCH-23 (#1184):
// when the persister channel saturates and the SinkFor closure drops a
// batch, the slog.Warn line MUST carry both `channel_used` and
// `channel_cap` so operators triaging starvation can tell whether the
// writer goroutine is wedged (used == cap, persistently) versus
// experiencing an instantaneous burst (used briefly == cap, then
// drains).
//
// Fixture mirrors TestPersister_FullChannel_Drops shape (small buffer,
// slow flush so the goroutine doesn't drain mid-test).
func TestPersister_FullChannel_LogsChannelUsed(t *testing.T) {
	cap := &captureHandler{}
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	p, _ := newTestPersister(t, func(o *Options) {
		o.ChannelBuffer = 1
		o.FlushInterval = 500 * time.Millisecond
	})
	sink := p.SinkFor("k")
	sink([]Entry{entry(t, 1, "u1")}, false)
	for i := 0; i < 256; i++ {
		sink([]Entry{entry(t, int64(i+2), fmt.Sprintf("u%d", i))}, false)
	}
	// Wait briefly for any pending drop log to land.
	time.Sleep(50 * time.Millisecond)

	records := cap.snapshot()
	var dropLine string
	for _, r := range records {
		if strings.HasPrefix(r, "event log persist: channel full") {
			dropLine = r
			break
		}
	}
	if dropLine == "" {
		t.Fatalf("expected at least one drop slog.Warn line, got %d records: %v", len(records), records)
	}
	if !strings.Contains(dropLine, "channel_used=") {
		t.Errorf("drop log missing channel_used attribute: %q", dropLine)
	}
	if !strings.Contains(dropLine, "channel_cap=") {
		t.Errorf("drop log missing channel_cap attribute: %q", dropLine)
	}
	if !strings.Contains(dropLine, "key=k") {
		t.Errorf("drop log missing key=k attribute: %q", dropLine)
	}
}
