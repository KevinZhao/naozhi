package session

import (
	"testing"
)

// TestRegisterCronStub_CreatesFreshStub locks the baseline: first call with
// a new key must install the session, mark dirty, and fire onChange.
func TestRegisterCronStub_CreatesFreshStub(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	var notified int
	r.SetOnChange(func() { notified++ })

	r.RegisterCronStub("cron:job-1", "/tmp/work", "initial prompt")

	if notified != 1 {
		t.Fatalf("onChange fired %d times on first stub, want 1", notified)
	}
	r.mu.RLock()
	_, ok := r.sessions["cron:job-1"]
	dirty := r.storeDirty
	r.mu.RUnlock()
	if !ok {
		t.Fatalf("cron stub was not registered")
	}
	if !dirty {
		t.Errorf("storeDirty should be true after RegisterCronStub(new)")
	}
}

// TestRegisterCronStub_NoOpOnIdenticalRefresh pins R176-PERF-P1 for cron:
// reloading cron.yaml re-invokes RegisterCronStub with the same workspace
// + prompt; those repeated calls must not mark the store dirty or bump the
// version. Prior behaviour fsynced sessions.json every save tick even when
// no user-observable state had changed.
//
// onChange IS allowed to fire on every refresh (cheap, and preserves the
// dashboard's "immediate sidebar kick after cron edit" UX). Only the
// expensive dirty/fsync path is gated on real mutation.
func TestRegisterCronStub_NoOpOnIdenticalRefresh(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	// Initial creation.
	r.RegisterCronStub("cron:job-2", "/w", "p")

	// Reset tracking to isolate the second call.
	r.SetOnChange(func() {})
	r.mu.Lock()
	r.storeDirty = false
	genBefore := r.storeGen.Load()
	r.mu.Unlock()

	// Reload with identical values — must NOT mark dirty / bump version.
	r.RegisterCronStub("cron:job-2", "/w", "p")

	r.mu.RLock()
	dirty := r.storeDirty
	r.mu.RUnlock()
	if dirty {
		t.Errorf("storeDirty flipped on identical RegisterCronStub refresh")
	}
	if got := r.storeGen.Load(); got != genBefore {
		t.Errorf("storeGen advanced on identical refresh: got %d, want %d", got, genBefore)
	}
}

// TestRegisterCronStub_DirtyOnActualChange is the "make sure we didn't
// starve real writes" counterpart: when the refresh changes workspace OR
// prompt, the full dirty + notify path must resume.
func TestRegisterCronStub_DirtyOnActualChange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		newWorkspace string
		newPrompt    string
	}{
		{"workspace_changed", "/new", "p"},
		{"prompt_changed", "/w", "p2"},
		{"both_changed", "/new", "p2"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newTestRouter(3)
			r.RegisterCronStub("cron:job-3", "/w", "p")

			var notified int
			r.SetOnChange(func() { notified++ })
			r.mu.Lock()
			r.storeDirty = false
			genBefore := r.storeGen.Load()
			r.mu.Unlock()

			r.RegisterCronStub("cron:job-3", c.newWorkspace, c.newPrompt)

			if notified != 1 {
				t.Errorf("onChange fired %d times on %s, want 1", notified, c.name)
			}
			r.mu.RLock()
			dirty := r.storeDirty
			r.mu.RUnlock()
			if !dirty {
				t.Errorf("storeDirty should be true after %s", c.name)
			}
			if got := r.storeGen.Load(); got == genBefore {
				t.Errorf("storeGen did not advance on %s", c.name)
			}
		})
	}
}

// TestRegisterCronStub_EmptyValuesDoNotClobber mirrors the existing
// production guard (`workspace != ""` / `lastPrompt != ""`): passing empty
// values must preserve the existing stub's fields and leave dirty/gen
// untouched (onChange may still fire — that is a cheap UI kick).
func TestRegisterCronStub_EmptyValuesDoNotClobber(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	r.RegisterCronStub("cron:job-4", "/keep", "keepme")

	r.SetOnChange(func() {})
	r.mu.Lock()
	r.storeDirty = false
	r.mu.Unlock()

	// Both empty — no data change expected.
	r.RegisterCronStub("cron:job-4", "", "")

	r.mu.RLock()
	dirty := r.storeDirty
	r.mu.RUnlock()
	if dirty {
		t.Errorf("storeDirty flipped on empty-values refresh")
	}
	if got := r.sessions["cron:job-4"].Workspace(); got != "/keep" {
		t.Errorf("workspace clobbered by empty refresh: got %q", got)
	}
	if got := loadStringAtomic(&r.sessions["cron:job-4"].lastPrompt); got != "keepme" {
		t.Errorf("lastPrompt clobbered by empty refresh: got %q", got)
	}
}
