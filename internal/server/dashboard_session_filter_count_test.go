package server

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// TestFilterAndCountSnapshots pins the contract extracted from handleList in
// R246-CR-002 (#736). The merged filter+count walk must:
//
//  1. drop dead sessions whose LastActive is older than 24h,
//  2. count running / ready across ALL surviving entries (so the maxProcs
//     pressure indicator stays correct even for scratch / cron / sys keys
//     that don't show up in the sidebar),
//  3. drop scratch / cron / sys keys from the returned slice.
//
// Splitting these three responsibilities apart is exactly the regression
// risk this test guards: a future "let's add another filter" patch could
// accidentally exclude scratch keys from the running count, masking a busy
// drawer session in the sidebar headline.
func TestFilterAndCountSnapshots(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour).UnixMilli()
	recent := now.Add(-1 * time.Hour).UnixMilli()

	in := []session.SessionSnapshot{
		// Sidebar-eligible: kept + counted.
		{Key: "feishu:p2p:alice", State: "running", LastActive: recent},
		{Key: "feishu:p2p:bob", State: "ready", LastActive: recent},
		// Dead but inside the 24h sidebar window: kept (no state count).
		{Key: "feishu:p2p:carol", State: "ended", DeathReason: "user_remove", LastActive: recent},
		// Dead and OUTSIDE 24h: dropped entirely (not kept, not counted).
		{Key: "feishu:p2p:dave", State: "ended", DeathReason: "ttl", LastActive: old},
		// Scratch is running — counted but NOT in returned slice.
		{Key: "scratch:x:y", State: "running", LastActive: recent},
		// Cron stub is ready — counted but NOT in returned slice.
		{Key: "cron:job-1", State: "ready", LastActive: recent},
		// sys daemon — neither counted (state is empty) nor returned.
		{Key: "sys:autotitler", State: "", LastActive: recent},
	}

	got, running, ready := filterAndCountSnapshots(in, now)

	if running != 2 { // alice + scratch
		t.Errorf("running = %d, want 2", running)
	}
	if ready != 2 { // bob + cron
		t.Errorf("ready = %d, want 2", ready)
	}

	// Build a key set for stable order-independent assertion.
	gotKeys := make(map[string]bool, len(got))
	for _, s := range got {
		gotKeys[s.Key] = true
	}
	wantKeys := []string{"feishu:p2p:alice", "feishu:p2p:bob", "feishu:p2p:carol"}
	if len(got) != len(wantKeys) {
		t.Fatalf("returned %d entries, want %d (keys: %v)", len(got), len(wantKeys), gotKeys)
	}
	for _, k := range wantKeys {
		if !gotKeys[k] {
			t.Errorf("expected key %q in result, got %v", k, gotKeys)
		}
	}
	for _, blocked := range []string{"feishu:p2p:dave", "scratch:x:y", "cron:job-1", "sys:autotitler"} {
		if gotKeys[blocked] {
			t.Errorf("blocked key %q leaked into sidebar", blocked)
		}
	}
}

// TestFilterAndCountSnapshotsEmpty guards the trivial path so a future
// "len(snapshots)==0 short-circuit" optimisation can't accidentally
// return the wrong slice header.
func TestFilterAndCountSnapshotsEmpty(t *testing.T) {
	got, running, ready := filterAndCountSnapshots(nil, time.Now())
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
	if running != 0 || ready != 0 {
		t.Errorf("running=%d ready=%d, want 0/0", running, ready)
	}
}
