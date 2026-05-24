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
	// snapshot deep-copy path (which heap-allocates a fresh *bool to
	// avoid sharing the underlying storage with mutating writers) is
	// exercised — a regression that copied the pointer directly would
	// pass the equality check below but tear under concurrent UpdateJob.
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
	// notify must be a fresh allocation, not the caller's pointer — a
	// regression that aliased j.Notify would let a concurrent UpdateJob
	// flip the value mid-execute and break the snapshot's "consistent
	// view" guarantee.
	if snap.notify == nil {
		t.Fatalf("notify: got nil, want pointer to true")
	}
	if snap.notify == j.Notify {
		t.Errorf("notify: snapshot aliases j.Notify; expected fresh allocation")
	}
	if *snap.notify != *j.Notify {
		t.Errorf("notify value: got %v want %v", *snap.notify, *j.Notify)
	}
}

// Compile-time anchor: make a future "where is jobSnapshot wired to
// Job?" grep land here. R236-ARCH-19.
var _ = (*Job)(nil)
