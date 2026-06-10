package cron

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
)

// captureHandler records every emitted log Record so a test can assert on
// structured fields. Minimal subset of slog.Handler — Enabled always true,
// no group/attr stacking (Stop's marshal-failure path emits one Error
// record with a flat attribute list).
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// TestStop_MarshalFailure_AlertKeyingField pins R246-GO-5 (#690): when
// Stop's marshal step fails, the slog.Error line MUST carry the explicit
// `persist:FAILED_DURING_SHUTDOWN` field so log aggregation can route the
// silent-data-loss case to a different alert channel than the recoverable
// per-mutation "save cron store" failure.
func TestStop_MarshalFailure_AlertKeyingField(t *testing.T) {
	cap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	withFailingMarshal(t, s)
	s.Stop()

	cap.mu.Lock()
	defer cap.mu.Unlock()
	found := false
	for _, r := range cap.records {
		if r.Level != slog.LevelError {
			continue
		}
		if r.Message != "marshal cron store on shutdown" {
			continue
		}
		var persistVal string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "persist" {
				persistVal = a.Value.String()
				return false
			}
			return true
		})
		if persistVal == "FAILED_DURING_SHUTDOWN" {
			found = true
			break
		}
		t.Errorf("found shutdown error log but persist field missing/wrong: got %q want %q",
			persistVal, "FAILED_DURING_SHUTDOWN")
	}
	if !found {
		t.Fatalf("expected slog.Error 'marshal cron store on shutdown' with persist=FAILED_DURING_SHUTDOWN; got %d records", len(cap.records))
	}
}
