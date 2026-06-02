package cron

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// warnCaptureHandler is a minimal slog.Handler that records WARN-level
// messages so tests can assert assertJobLockHeld fired its contract probe.
type warnCaptureHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *warnCaptureHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn
}

func (h *warnCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *warnCaptureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *warnCaptureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *warnCaptureHandler) sawJobLockWarn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, "jobLock not held by caller") {
			return true
		}
	}
	return false
}

func (h *warnCaptureHandler) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = nil
}

// TestRunStore_LockHierarchy_RuntimeChecked pins R242-ARCH-12 (#753): the
// jobLock hierarchy used to be enforced only by godoc on cacheHeadPush,
// trimSkipFromCache, and cacheTrimAfterDisk. Each now calls
// assertJobLockHeld, so a caller that violates the *Locked contract is
// caught at runtime (under `go test`, where testing.Testing() is true)
// rather than surfacing as a silent cache↔disk race.
//
// We swap the default slog handler for a warn-capturing one, invoke each
// helper WITHOUT holding jobLock, and assert the contract probe fired.
// The mirror "held → no warn" branch confirms the probe is not a false
// positive on the legitimate locked path.
func TestRunStore_LockHierarchy_RuntimeChecked(t *testing.T) {
	cap := &warnCaptureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := newRunStore(storePath, 10, time.Hour)
	if s == nil || s.disabled {
		t.Fatalf("newRunStore must succeed; got disabled")
	}
	jobID := mustGenerateID()
	// Warm the cache entry so the helpers reach their body (some return
	// early on a cold/missing entry before doing real work, but the
	// assertJobLockHeld call is the first statement so it fires regardless).
	s.warmCacheLocked(jobID)

	now := time.Now()
	cutoff := now.Add(-time.Hour)

	type tc struct {
		name string
		call func()
	}
	cases := []tc{
		{"cacheHeadPush", func() { s.cacheHeadPush(jobID, CronRunSummary{RunID: mustGenerateRunID()}) }},
		{"trimSkipFromCache", func() { s.trimSkipFromCache(jobID, now) }},
		{"cacheTrimAfterDisk", func() { s.cacheTrimAfterDisk(jobID, cutoff) }},
	}

	for _, c := range cases {
		t.Run("unheld/"+c.name, func(t *testing.T) {
			cap.reset()
			c.call()
			if !cap.sawJobLockWarn() {
				t.Fatalf("%s called without jobLock must emit the contract warn (#753 runtime check missing)", c.name)
			}
		})
	}

	for _, c := range cases {
		t.Run("held/"+c.name, func(t *testing.T) {
			cap.reset()
			lock := s.jobLock(jobID)
			lock.Lock()
			c.call()
			lock.Unlock()
			if cap.sawJobLockWarn() {
				t.Fatalf("%s with jobLock held must NOT emit the contract warn (false positive)", c.name)
			}
		})
	}
}
