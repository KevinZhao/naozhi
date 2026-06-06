package cron

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// TestFreshReapEmptySessionIDLogsDiagnostic pins #1845: when a fresh_context
// cron run succeeds but the CLI never emitted a session-id (result.SessionID
// == ""), the reap still re-registers a sidebar stub — but registerStubByValue
// treats the empty ID as a no-op chain, so the stub carries NO clickable JSONL
// history. Without a log line this missing-history case is silently
// indistinguishable (to an operator) from "the reap never ran". The reap MUST
// emit a debug diagnostic so the two are distinguishable.
func TestFreshReapEmptySessionIDLogsDiagnostic(t *testing.T) {
	// NOT t.Parallel(): mutates the process-wide default slog logger.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	rec := &recordingBroadcaster{}
	// Empty sid → okSession.Send returns SessionID="" → result.SessionID == "".
	router := &reapRouter{sid: ""}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router, Telemetry: rec})

	j := &Job{ID: "job-empty-sid", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow */)

	if got := rec.endedAtCron(0); got.State != RunStateSucceeded {
		t.Fatalf("state: want succeeded, got %q (err=%q)", got.State, got.ErrorClass)
	}

	// The stub must still be re-registered (existing reap behaviour), but with
	// an empty chain — and the diagnostic must have fired.
	wantKey := sessionkey.CronKey(j.ID)
	_, regs := router.snapshot()
	var reapStub *stubCall
	for i := len(regs) - 1; i >= 0; i-- {
		if regs[i].key == wantKey {
			reapStub = &regs[i]
			break
		}
	}
	if reapStub == nil {
		t.Fatalf("no stub re-registered for %q after reap; regs=%v", wantKey, regs)
	}
	if len(reapStub.chainIDs) != 0 {
		t.Errorf("empty session_id reap stub chainIDs = %v, want empty (no history chain)", reapStub.chainIDs)
	}

	if out := buf.String(); !strings.Contains(out, "empty session_id on successful run") {
		t.Errorf("missing #1845 empty-session_id diagnostic log\nfull log:\n%s", out)
	}
}

// TestFreshReapNonEmptySessionIDNoDiagnostic is the negative half of #1845:
// the empty-session_id diagnostic must NOT fire on the normal path where the
// CLI did emit a session id, otherwise the log loses its signal value.
func TestFreshReapNonEmptySessionIDNoDiagnostic(t *testing.T) {
	// NOT t.Parallel(): mutates the process-wide default slog logger.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	rec := &recordingBroadcaster{}
	router := &reapRouter{sid: "sess-fresh-ok"}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router, Telemetry: rec})

	j := &Job{ID: "job-ok-sid", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true)

	if got := rec.endedAtCron(0); got.State != RunStateSucceeded {
		t.Fatalf("state: want succeeded, got %q (err=%q)", got.State, got.ErrorClass)
	}

	if out := buf.String(); strings.Contains(out, "empty session_id on successful run") {
		t.Errorf("empty-session_id diagnostic fired on non-empty session id path\nfull log:\n%s", out)
	}
}
