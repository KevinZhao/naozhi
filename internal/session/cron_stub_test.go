package session

import (
	"slices"
	"strconv"
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
	if got := loadAtomicString(&r.sessions["cron:job-4"].lastPrompt); got != "keepme" {
		t.Errorf("lastPrompt clobbered by empty refresh: got %q", got)
	}
}

// TestRegisterCronStubWithChain_SetsChainOnFreshStub 固化 fresh stub 注入
// chain 的契约：无 chain 的旧路径会得到 nil prevSessionIDs；带 chain 时
// 必须 Clone 进 ManagedSession（不共享底层数组，防 caller 改 slice
// 污染 router 内部状态）。
func TestRegisterCronStubWithChain_SetsChainOnFreshStub(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	chain := []string{"sess-aaa", "sess-bbb"}

	r.RegisterCronStubWithChain("cron:job-c1", "/w", "p", chain)

	r.mu.RLock()
	s := r.sessions["cron:job-c1"]
	r.mu.RUnlock()
	if s == nil {
		t.Fatal("stub not registered")
	}
	if !slices.Equal(s.prevSessionIDs, chain) {
		t.Errorf("prevSessionIDs = %v, want %v", s.prevSessionIDs, chain)
	}
	// Caller mutation of the passed slice must not affect router state.
	chain[0] = "hijacked"
	if s.prevSessionIDs[0] == "hijacked" {
		t.Errorf("caller-side mutation bled into router: prevSessionIDs shares backing array")
	}
}

// TestRegisterCronStubWithChain_NoOpOnIdenticalChain pins that a reload with
// the same chainIDs does not mark the store dirty or bump the generation.
// Cron's stubRefresh goroutine is paved over onExecute; repeat calls with
// unchanged chain are the steady-state case, which must stay fsync-free.
func TestRegisterCronStubWithChain_NoOpOnIdenticalChain(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	r.RegisterCronStubWithChain("cron:job-c2", "/w", "p", []string{"sess-xxx"})

	r.SetOnChange(func() {})
	r.mu.Lock()
	r.storeDirty = false
	genBefore := r.storeGen.Load()
	r.mu.Unlock()

	r.RegisterCronStubWithChain("cron:job-c2", "/w", "p", []string{"sess-xxx"})

	r.mu.RLock()
	dirty := r.storeDirty
	r.mu.RUnlock()
	if dirty {
		t.Errorf("storeDirty flipped on identical chain refresh")
	}
	if got := r.storeGen.Load(); got != genBefore {
		t.Errorf("storeGen advanced on identical chain refresh: got %d, want %d", got, genBefore)
	}
}

// TestRegisterCronStubWithChain_DirtyOnChainChange covers the new code path:
// when the chain actually changes (cron recorded a new LastSessionID), the
// stub's prevSessionIDs must be updated in place and the store marked
// dirty so sessions.json persists the new chain on the next save tick.
func TestRegisterCronStubWithChain_DirtyOnChainChange(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	r.RegisterCronStubWithChain("cron:job-c3", "/w", "p", []string{"sess-old"})

	r.SetOnChange(func() {})
	r.mu.Lock()
	r.storeDirty = false
	genBefore := r.storeGen.Load()
	r.mu.Unlock()

	newChain := []string{"sess-new"}
	r.RegisterCronStubWithChain("cron:job-c3", "/w", "p", newChain)

	r.mu.RLock()
	dirty := r.storeDirty
	s := r.sessions["cron:job-c3"]
	r.mu.RUnlock()
	if !dirty {
		t.Errorf("storeDirty should be true after chain change")
	}
	if got := r.storeGen.Load(); got == genBefore {
		t.Errorf("storeGen did not advance on chain change")
	}
	if !slices.Equal(s.prevSessionIDs, newChain) {
		t.Errorf("prevSessionIDs = %v, want %v", s.prevSessionIDs, newChain)
	}
}

// TestRegisterCronStubWithChain_NilChainLeavesExistingChain fixes the legacy
// compatibility rule: the nil/empty chain branch (how RegisterCronStub 走到
// 这里) must NOT wipe an already-recorded chain. Otherwise the old
// RegisterCronStub signature (used by legacy integrations or tests) would
// silently blow away cron history lookup on every reload.
func TestRegisterCronStubWithChain_NilChainLeavesExistingChain(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	r.RegisterCronStubWithChain("cron:job-c4", "/w", "p", []string{"sess-keep"})

	r.RegisterCronStub("cron:job-c4", "/w", "p") // equivalent to nil chain

	r.mu.RLock()
	s := r.sessions["cron:job-c4"]
	r.mu.RUnlock()
	if !slices.Equal(s.prevSessionIDs, []string{"sess-keep"}) {
		t.Errorf("nil chain wiped existing prevSessionIDs: got %v", s.prevSessionIDs)
	}
}

// TestRegisterCronStub_OverSubQuotaStillRegisters pins the R242-ARCH-2
// (#720) stub-path behaviour: the per-namespace exempt sub-quota gate
// lives in spawnSession, but stub registration never spawns a process, so
// it must NOT hard-reject when the cron bucket is already at its cap.
// Dropping an over-quota cron stub would lose its history chain and break
// the dashboard event panel; the fix only logs the over-quota condition.
// This test guards that every stub past the cap still installs.
func TestRegisterCronStub_OverSubQuotaStillRegisters(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)

	// Register one more stub than the cron sub-quota allows. Each stub is
	// alive (exempt, no process) so countExemptByKind counts it.
	total := maxCronExempt + 3
	for i := 0; i < total; i++ {
		key := "cron:job-over-" + strconv.Itoa(i)
		r.RegisterCronStub(key, "/w", "p")
	}

	r.mu.RLock()
	got := 0
	for k := range r.sessions {
		if exemptKind(k) == "cron" {
			got++
		}
	}
	r.mu.RUnlock()
	if got != total {
		t.Fatalf("over-quota cron stubs were dropped: registered %d, want %d", got, total)
	}
}
