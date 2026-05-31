package server

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// fakeCronPromptSaver records SetJobPrompt calls and returns a programmed err.
type fakeCronPromptSaver struct {
	err        error
	lastJobID  string
	lastPrompt string
	calls      int
}

func (f *fakeCronPromptSaver) EnsureStub(string) bool           { return false }
func (f *fakeCronPromptSaver) KnownSessionIDs() map[string]bool { return nil }
func (f *fakeCronPromptSaver) SetJobPrompt(jobID, prompt string) error {
	f.calls++
	f.lastJobID = jobID
	f.lastPrompt = prompt
	return f.err
}

type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestAutoSaveCronPrompt_SuppressesAlreadySet pins R20260531-CR-001: after the
// first turn writes the prompt, every later IM turn gets ErrPromptAlreadySet
// from SetJobPrompt. That sentinel is benign auto-save behaviour and MUST NOT
// emit a Warn line — otherwise each cron turn floods the journal.
func TestAutoSaveCronPrompt_SuppressesAlreadySet(t *testing.T) {
	var lb lockedBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lb, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	saver := &fakeCronPromptSaver{err: cron.ErrPromptAlreadySet}
	h := &Hub{scheduler: saver}

	key := sessionkey.CronKey("job123")
	h.autoSaveCronPrompt("send", key, "do the thing")

	if saver.calls != 1 {
		t.Fatalf("SetJobPrompt calls = %d, want 1", saver.calls)
	}
	if saver.lastJobID != "job123" {
		t.Fatalf("jobID = %q, want %q (prefix must be trimmed)", saver.lastJobID, "job123")
	}
	if strings.Contains(lb.String(), "set cron prompt") {
		t.Fatalf("ErrPromptAlreadySet must be suppressed, got Warn line: %s", lb.String())
	}
}

// TestAutoSaveCronPrompt_WarnsOnRealError pins that any non-benign SetJobPrompt
// failure still surfaces at Warn — the suppression is sentinel-specific.
func TestAutoSaveCronPrompt_WarnsOnRealError(t *testing.T) {
	var lb lockedBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lb, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	saver := &fakeCronPromptSaver{err: errors.New("disk full")}
	h := &Hub{scheduler: saver}

	h.autoSaveCronPrompt("passthrough", sessionkey.CronKey("jobX"), "txt")

	out := lb.String()
	if !strings.Contains(out, "passthrough: set cron prompt") {
		t.Fatalf("expected Warn line for non-sentinel error, got: %s", out)
	}
}

// TestAutoSaveCronPrompt_NoSchedulerOrNonCron is a no-op guard: without a
// scheduler, or for a non-cron key, SetJobPrompt must never be called.
func TestAutoSaveCronPrompt_NoSchedulerOrNonCron(t *testing.T) {
	// nil scheduler.
	(&Hub{}).autoSaveCronPrompt("send", sessionkey.CronKey("j"), "t")

	saver := &fakeCronPromptSaver{}
	h := &Hub{scheduler: saver}
	h.autoSaveCronPrompt("send", "feishu:p2p:abc", "t") // non-cron key
	if saver.calls != 0 {
		t.Fatalf("SetJobPrompt called for non-cron key / nil scheduler: calls=%d", saver.calls)
	}
}
