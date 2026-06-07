package cron

import (
	"testing"
	"time"
)

// TestJobSnapshotCoversExecuteFields is a contract test for R236-ARCH-19.
//
// jobSnapshot mirrors the subset of *Job that executeOpt / deliverNotice
// read after the s.mu read-side critical section ends. The shape is
// maintained by hand: when a new Job field is added, snapshotJob must be
// updated in lockstep, otherwise executeOpt either silently misses the
// new value or — worse — falls back to dereferencing *j outside the lock
// and races concurrent SetJobPrompt / UpdateJob writers.
//
// This test exercises snapshotJob with every executeOpt-relevant Job
// field set to a distinguishable non-zero value and asserts each one
// landed in the corresponding snapshot field. It does NOT cover purely
// persistent fields (RunCounters / LastResult / LastSessionID etc.) —
// those are intentionally read directly from *Job under s.mu by
// recordResult / persist paths and never copied into jobSnapshot.
//
// Adding a new Job field that executeOpt should observe lock-free?
//  1. Add the field to jobSnapshot + snapshotJob.
//  2. Add it to the literal below + extend the assertions block.
//
// The compile-time `_ = (*Job)(nil)` line at the bottom makes a future
// "does this struct exist?" grep cheap and pins the import: tests that
// remove the Job type will break this anchor first.
func TestJobSnapshotCoversExecuteFields(t *testing.T) {
	// notify uses tri-state pointer semantics; explicitly true so the
	// snapshot's notify aliasing path is exercised. R090135-PERF-003
	// (#1931): the snapshot now aliases j.Notify directly (alloc-free)
	// rather than deep-copying — safe because UpdateJob reassigns the
	// whole pointer under s.mu and never mutates *j.Notify in place, so
	// the pointed-to bool is immutable once published.
	tru := true
	j := &Job{
		ID:             "abcdef0123456789",
		Schedule:       "@every 10m",
		Prompt:         "snapshot-prompt",
		Platform:       "feishu",
		ChatID:         "chat-1",
		ChatType:       "group",
		CreatedBy:      "tester",
		CreatedAt:      time.Now(),
		Title:          "Snapshot Contract",
		WorkDir:        "/tmp/snapshot-contract",
		Backend:        "claude",
		NotifyPlatform: "feishu",
		NotifyChatID:   "notify-chat",
		Notify:         &tru,
		FreshContext:   true,
	}

	s := &Scheduler{}
	snap := s.snapshotJob(j)

	if snap.jobID != j.ID {
		t.Errorf("jobID: got %q want %q", snap.jobID, j.ID)
	}
	if snap.prompt != j.Prompt {
		t.Errorf("prompt: got %q want %q", snap.prompt, j.Prompt)
	}
	if snap.workDir != j.WorkDir {
		t.Errorf("workDir: got %q want %q", snap.workDir, j.WorkDir)
	}
	if snap.platName != j.Platform {
		t.Errorf("platName: got %q want %q", snap.platName, j.Platform)
	}
	if snap.chatID != j.ChatID {
		t.Errorf("chatID: got %q want %q", snap.chatID, j.ChatID)
	}
	if snap.notifyPlat != j.NotifyPlatform {
		t.Errorf("notifyPlat: got %q want %q", snap.notifyPlat, j.NotifyPlatform)
	}
	if snap.notifyChat != j.NotifyChatID {
		t.Errorf("notifyChat: got %q want %q", snap.notifyChat, j.NotifyChatID)
	}
	if snap.fresh != j.FreshContext {
		t.Errorf("fresh: got %v want %v", snap.fresh, j.FreshContext)
	}
	if snap.schedule != j.Schedule {
		t.Errorf("schedule: got %q want %q", snap.schedule, j.Schedule)
	}
	if snap.backend != j.Backend {
		t.Errorf("backend: got %q want %q", snap.backend, j.Backend)
	}
	if snap.label == "" {
		t.Errorf("label: got empty, want non-empty derived from Title=%q", j.Title)
	}
	// R090135-PERF-003 (#1931): notify now aliases j.Notify directly to
	// avoid a per-tick *bool heap alloc. This is tear-free because
	// UpdateJob (scheduler_jobs.go) reassigns j.Notify to a fresh &v / nil
	// under s.mu.Lock and never mutates *j.Notify in place — the snapshot's
	// RLock read therefore captures a stable pointer to an immutable bool.
	if snap.notify == nil {
		t.Fatalf("notify: got nil, want pointer to true")
	}
	if snap.notify != j.Notify {
		t.Errorf("notify: snapshot should alias j.Notify (alloc-free); got distinct pointer")
	}
	if *snap.notify != *j.Notify {
		t.Errorf("notify value: got %v want %v", *snap.notify, *j.Notify)
	}
	// nil Notify must round-trip as nil (tri-state "unset").
	jNil := &Job{ID: "0123456789abcdef", Schedule: "@every 5m", Prompt: "p", Platform: "feishu", ChatID: "c"}
	if snapNil := s.snapshotJob(jNil); snapNil.notify != nil {
		t.Errorf("notify: nil Job.Notify must snapshot as nil, got %v", *snapNil.notify)
	}
}

// TestSnapshotJobLockedMirrorsSnapshotJob is the R20260528-PERF-2 (#1351)
// fold contract test. snapshotJobLocked is the lock-held variant
// executeOpt's jitter block uses to fold the post-jitter recheck and
// snapshot copy into one RLock window. Any divergence between the two
// outputs would mean the jitter path observes a different view of *j
// than the no-jitter path — silent semantic drift the existing
// snapshotJob test would not catch since it only exercises the
// public method.
func TestSnapshotJobLockedMirrorsSnapshotJob(t *testing.T) {
	tru := true
	j := &Job{
		ID:             "abcdef0123456789",
		Schedule:       "@every 10m",
		Prompt:         "fold-snapshot-prompt",
		Platform:       "feishu",
		ChatID:         "chat-fold",
		WorkDir:        "/tmp/fold-snapshot",
		Backend:        "claude",
		NotifyPlatform: "feishu",
		NotifyChatID:   "notify-fold",
		Title:          "Fold Snapshot",
		Notify:         &tru,
		FreshContext:   true,
	}

	s := &Scheduler{}
	viaPublic := s.snapshotJob(j)

	// snapshotJobLocked requires the caller to hold s.mu — emulate
	// executeOpt's jitter block by RLock straddling the call.
	s.mu.RLock()
	viaLocked := snapshotJobLocked(j)
	s.mu.RUnlock()

	if viaPublic.jobID != viaLocked.jobID ||
		viaPublic.prompt != viaLocked.prompt ||
		viaPublic.workDir != viaLocked.workDir ||
		viaPublic.platName != viaLocked.platName ||
		viaPublic.chatID != viaLocked.chatID ||
		viaPublic.notifyPlat != viaLocked.notifyPlat ||
		viaPublic.notifyChat != viaLocked.notifyChat ||
		viaPublic.schedule != viaLocked.schedule ||
		viaPublic.backend != viaLocked.backend ||
		viaPublic.label != viaLocked.label ||
		viaPublic.fresh != viaLocked.fresh ||
		viaPublic.lastSessionID != viaLocked.lastSessionID {
		t.Fatalf("snapshotJobLocked diverged from snapshotJob:\n  public=%+v\n  locked=%+v",
			viaPublic, viaLocked)
	}
	// R090135-PERF-003 (#1931): both paths alias j.Notify (alloc-free).
	// They must agree on pointer identity AND value so the jitter and
	// no-jitter execute paths observe the same notify decision input.
	if viaPublic.notify == nil || viaLocked.notify == nil {
		t.Fatal("both snapshots must populate notify")
	}
	if viaPublic.notify != j.Notify || viaLocked.notify != j.Notify {
		t.Fatal("both snapshots must alias j.Notify (alloc-free)")
	}
	if *viaPublic.notify != *viaLocked.notify {
		t.Fatalf("notify value diverged: public=%v locked=%v", *viaPublic.notify, *viaLocked.notify)
	}
}

// Compile-time anchor: make a future "where is jobSnapshot wired to
// Job?" grep land here. R236-ARCH-19.
var _ = (*Job)(nil)
