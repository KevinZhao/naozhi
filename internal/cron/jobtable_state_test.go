package cron

import (
	"errors"
	"testing"
	"time"
)

func TestJobTableLoad(t *testing.T) {
	tbl := tableWith(t)
	restored := []*Job{
		{ID: "a", Schedule: "@every 5m", Platform: "p", ChatID: "c", Prompt: "pa", WorkDir: "/w", LastSessionID: "s"},
		{ID: "b", Schedule: "@every 5m", Platform: "p", ChatID: "c", Paused: true},
		{ID: "bad", Schedule: "never", Platform: "p", ChatID: "d"},
		{ID: "chat-over", Schedule: "@every 5m", Platform: "p", ChatID: "c"},
		{ID: "d", Schedule: "@every 5m", Platform: "p", ChatID: "d"},
		{ID: "cap-over", Schedule: "@every 5m", Platform: "p", ChatID: "e"},
	}
	r := tbl.load(restored, 3, 2)
	if r.loaded != 3 || r.skippedOverCap != 1 || r.skippedOverPerChat != 1 {
		t.Errorf("loaded %d, over cap %d, over per-chat %d; want 3, 1, 1", r.loaded, r.skippedOverCap, r.skippedOverPerChat)
	}
	for _, id := range []string{"a", "b", "d"} {
		if _, ok := tbl.jobs[id]; !ok {
			t.Errorf("%s not loaded", id)
		}
	}
	if len(tbl.sortedJobIDs) != 3 || tbl.chatJobCount[chatKeyFor("p", "c")] != 2 {
		t.Error("loaded jobs not indexed")
	}
	// Active jobs get a plan, paused ones none; every loaded job gets a stub.
	if len(r.plans) != 2 || r.plans[0].jobID != "a" || r.plans[1].jobID != "d" {
		t.Errorf("plans = %+v, want a and d", r.plans)
	}
	if len(r.stubs) != 3 || r.stubs[0] != (jobStubFields{id: "a", workDir: "/w", prompt: "pa", lastSessionID: "s"}) {
		t.Errorf("stubs = %+v", r.stubs)
	}
}

func TestJobTableRecordResult(t *testing.T) {
	tbl, active, _ := mutateTable(t)
	active.LastSessionID = "old"
	end := time.Unix(1700000000, 0)
	c, ok := tbl.recordResult(active.ID, terminalRecord{endedAt: end, result: "r", errMsg: "e", sessionID: "new", errClass: ErrorClass("x"), state: RunStateFailed})
	if !ok || !c.sessionChanged || c.prev.LastSessionID != "old" {
		t.Fatalf("record: ok=%v change=%+v", ok, c)
	}
	if !active.LastRunAt.Equal(end) || active.LastResult != "r" || active.LastError != "e" || active.LastErrorClass != "x" ||
		active.LastSessionID != "new" || active.RunCounters.Failed != 1 || active.RunCounters.Total != 1 {
		t.Errorf("after record: %+v", active)
	}
	if len(c.snap.entries) != 2 || c.snap.seq != 1 || c.snap.entries[0].LastResult != "r" {
		t.Errorf("snapshot: %d entries, seq %d; want 2 carrying the result at seq 1", len(c.snap.entries), c.snap.seq)
	}
	// No session id keeps the old one and is no change; the same one is none either.
	if c, _ := tbl.recordResult(active.ID, terminalRecord{}); c.sessionChanged || active.LastSessionID != "new" {
		t.Errorf("empty session id: changed=%v session=%q", c.sessionChanged, active.LastSessionID)
	}
	if c, _ := tbl.recordResult(active.ID, terminalRecord{sessionID: "new"}); c.sessionChanged {
		t.Error("the same session id reported as a change")
	}
	if _, ok := tbl.recordResult("nope", terminalRecord{}); ok {
		t.Error("record on a missing job reported ok")
	}

	tbl.revertResult(active.ID, c.prev)
	if active.LastSessionID != "old" || active.LastResult != "" || active.RunCounters.Total != 0 {
		t.Errorf("after revert: %+v", active)
	}
	tbl.revertResult("nope", c.prev) // a job deleted in between: no panic, no effect
}

func TestJobTablePersist(t *testing.T) {
	tbl, _, _ := mutateTable(t)
	m, err := tbl.persist()
	if err != nil || m.seq != 1 || len(m.data) == 0 {
		t.Errorf("persist: seq %d, %d bytes, err %v", m.seq, len(m.data), err)
	}
	if !tbl.mu.TryLock() {
		t.Fatal("persist left the table locked")
	}
	tbl.mu.Unlock()
	failMarshal(tbl)
	if _, err := tbl.persist(); !errors.Is(err, ErrPersistFailed) {
		t.Errorf("err = %v, want ErrPersistFailed", err)
	}
}

func TestJobTableEntryIDOf(t *testing.T) {
	tbl, active, _ := mutateTable(t)
	for _, tc := range []struct {
		name string
		j    *Job
		want cronEntryID
	}{
		{"own entry", &Job{ID: "other", entryID: 3}, 3},
		{"detached copy falls back", &Job{ID: active.ID}, 7},
		{"unregistered", &Job{ID: "nope"}, 0},
		{"no id", &Job{}, 0},
	} {
		if got := tbl.entryIDOf(tc.j); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestJobTableTriggerable(t *testing.T) {
	tbl, active, paused := mutateTable(t)
	active.Prompt = "go"
	if err := tbl.triggerable(active.ID); err != nil {
		t.Errorf("active: %v", err)
	}
	paused.Prompt = "go"
	if err := tbl.triggerable(paused.ID); !errors.Is(err, ErrJobPaused) {
		t.Errorf("paused: %v", err)
	}
	active.Prompt = ""
	if err := tbl.triggerable(active.ID); !errors.Is(err, ErrJobNoPrompt) {
		t.Errorf("no prompt: %v", err)
	}
	if err := tbl.triggerable("nope"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("missing: %v", err)
	}
}
