package cron

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// boolPtr is defined elsewhere in the package test suite; sideEffectsJob adds a
// sandbox job with side_effects=true so the §6.2 queue paths fire.
func sideEffectsJob(t *testing.T, s *Scheduler) *Job {
	t.Helper()
	se := true
	j := NewJobFull(JobInit{
		Schedule:    "@daily",
		Prompt:      "push a PR",
		IM:          JobIMContext{Platform: "dashboard", ChatID: "global"},
		Placement:   PlacementSandbox,
		SideEffects: &se,
	})
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	return j
}

// TestAttention_TransportFailureEnqueuesSideEffectingJob: a side-effecting
// sandbox job that ends failed-transport (stream lost, Stop unconfirmed) lands
// in the §7.4 confirmation queue with reason=transport.
func TestAttention_TransportFailureEnqueuesSideEffectingJob(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{
		outcome: SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: "stream reset", StopConfirmed: false},
	}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	items := s.ListSandboxAttention()
	if len(items) != 1 {
		t.Fatalf("queue len = %d, want 1 (side-effecting transport failure must enqueue)", len(items))
	}
	if items[0].Reason != attentionReasonTransport {
		t.Errorf("reason = %q, want %q", items[0].Reason, attentionReasonTransport)
	}
	if items[0].JobID != j.ID {
		t.Errorf("jobID = %q, want %q", items[0].JobID, j.ID)
	}
	if s.SandboxAttentionCount() != 1 {
		t.Errorf("count = %d, want 1", s.SandboxAttentionCount())
	}
}

// TestAttention_TransportFailureNoSideEffectsSkipsQueue: a transport failure on
// a job WITHOUT side_effects must NOT enter the queue — it is safe to re-run
// freely (§6.2 rule 3 only fences side-effecting jobs).
func TestAttention_TransportFailureNoSideEffectsSkipsQueue(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{
		outcome: SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: "stream reset"},
	}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s) // no side effects

	s.executeOpt(j, true)
	waitEnded(t, rec)

	if n := s.SandboxAttentionCount(); n != 0 {
		t.Fatalf("queue count = %d, want 0 (no-side-effect transport failure must NOT enqueue)", n)
	}
}

// TestAttention_SuccessDoesNotEnqueue: a clean success never enters the queue,
// even for a side-effecting job.
func TestAttention_SuccessDoesNotEnqueue(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{
		lines:   []string{`{"kind":"cli","line":{"type":"result","is_error":false,"result":"done"}}`},
		outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "done"},
	}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	if n := s.SandboxAttentionCount(); n != 0 {
		t.Fatalf("queue count = %d, want 0 (success must not enqueue)", n)
	}
}

// TestConfirmSandboxRun_RemovesFromQueue: ConfirmSandboxRun resolves a queue
// item without replaying.
func TestConfirmSandboxRun_RemovesFromQueue(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	s.writeSandboxAttention(sandboxAttention{
		JobID: "0123456789abcdef", RunID: "feedfacefeedface",
		Reason: attentionReasonTransport, StartedAtMS: time.Now().UnixMilli(),
		CreatedAtMS: time.Now().UnixMilli(),
	}, slog.Default())

	if s.SandboxAttentionCount() != 1 {
		t.Fatalf("precondition: expected 1 queued")
	}
	if err := s.ConfirmSandboxRun("feedfacefeedface"); err != nil {
		t.Fatalf("ConfirmSandboxRun: %v", err)
	}
	if s.SandboxAttentionCount() != 0 {
		t.Fatalf("confirm must remove the record")
	}
	// Idempotent: confirming again is a no-op, not an error.
	if err := s.ConfirmSandboxRun("feedfacefeedface"); err != nil {
		t.Fatalf("idempotent confirm should not error: %v", err)
	}
}

// TestConfirmSandboxRun_InvalidID rejects a non-hex run id.
func TestConfirmSandboxRun_InvalidID(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)
	if err := s.ConfirmSandboxRun("../etc/passwd"); !errors.Is(err, errInvalidAttentionID) {
		t.Fatalf("err = %v, want errInvalidAttentionID", err)
	}
}

// TestDeleteJobAttention_ClearsQueue: deleting a job drops its queue records.
func TestDeleteJobAttention_ClearsQueue(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	s.writeSandboxAttention(sandboxAttention{
		JobID: "0123456789abcdef", RunID: "1111111111111111",
		Reason: attentionReasonTransport, CreatedAtMS: time.Now().UnixMilli(),
	}, slog.Default())
	s.writeSandboxAttention(sandboxAttention{
		JobID: "aaaaaaaaaaaaaaaa", RunID: "2222222222222222",
		Reason: attentionReasonOrphaned, CreatedAtMS: time.Now().UnixMilli(),
	}, slog.Default())

	s.deleteJobAttention("0123456789abcdef")

	items := s.ListSandboxAttention()
	if len(items) != 1 || items[0].JobID != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("delete must drop only the deleted job's records; got %+v", items)
	}
}

// TestListSandboxAttention_NewestFirst pins that ListSandboxAttention returns
// records ordered by CreatedAtMS descending (newest first).
// R20260613-PERF-7: the sort was ported from sort.Slice to slices.SortFunc;
// this test ensures the descending order is preserved.
func TestListSandboxAttention_NewestFirst(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	base := time.Now().UnixMilli()
	s.writeSandboxAttention(sandboxAttention{
		JobID: "0123456789abcdef", RunID: "aaaaaaaaaaaaaaaa",
		Reason: attentionReasonTransport, CreatedAtMS: base,
	}, slog.Default())
	s.writeSandboxAttention(sandboxAttention{
		JobID: "0123456789abcdef", RunID: "bbbbbbbbbbbbbbbb",
		Reason: attentionReasonOrphaned, CreatedAtMS: base + 1000,
	}, slog.Default())
	s.writeSandboxAttention(sandboxAttention{
		JobID: "0123456789abcdef", RunID: "cccccccccccccccc",
		Reason: attentionReasonTransport, CreatedAtMS: base + 500,
	}, slog.Default())

	items := s.ListSandboxAttention()
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d", len(items))
	}
	// Expect descending: base+1000, base+500, base
	if items[0].RunID != "bbbbbbbbbbbbbbbb" || items[1].RunID != "cccccccccccccccc" || items[2].RunID != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("wrong order: %v %v %v", items[0].RunID, items[1].RunID, items[2].RunID)
	}
}

// TestListSandboxAttention_SkipsCorrupt: a corrupt queue file does not hide the
// rest of the queue.
func TestListSandboxAttention_SkipsCorrupt(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	s.writeSandboxAttention(sandboxAttention{
		JobID: "0123456789abcdef", RunID: "1111111111111111",
		Reason: attentionReasonTransport, CreatedAtMS: time.Now().UnixMilli(),
	}, slog.Default())
	// Drop a corrupt file alongside it.
	if err := os.WriteFile(filepath.Join(s.sandboxAttentionDir(), "garbage.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	items := s.ListSandboxAttention()
	if len(items) != 1 {
		t.Fatalf("corrupt file must be skipped, valid one kept; got %d items", len(items))
	}
}
