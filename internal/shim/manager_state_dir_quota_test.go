package shim

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckStateDirQuota_DisabledByDefault confirms the legacy contract:
// callers that do not set StateDirQuotaBytes see no quota gate. Any pre-
// existing state-dir contents are accepted regardless of size.
//
// RNEW-OPS-415 (#456) minimal slice — keeps existing deployments unchanged.
func TestCheckStateDirQuota_DisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	// Write 4 KiB of bytes to make the dir clearly non-empty.
	if err := os.WriteFile(filepath.Join(dir, "a.json"), make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	if err := m.checkStateDirQuota(); err != nil {
		t.Fatalf("default quota=0 must accept any size, got %v", err)
	}
}

// TestCheckStateDirQuota_BlocksWhenOverLimit confirms the gate fires when
// the existing on-disk size already meets/exceeds the configured quota.
// The error must wrap ErrStateDirQuotaExceeded so callers can use errors.Is
// to map it onto a distinct user-visible message ("clean state dir / raise
// quota") versus the existing ErrMaxShims path ("transient: another
// session must exit").
func TestCheckStateDirQuota_BlocksWhenOverLimit(t *testing.T) {
	dir := t.TempDir()
	// 8 KiB of state.
	if err := os.WriteFile(filepath.Join(dir, "fat.json"), make([]byte, 8192), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := mustNewManager(t, ManagerConfig{
		StateDir:           dir,
		StateDirQuotaBytes: 4096,
	})
	err := m.checkStateDirQuota()
	if err == nil {
		t.Fatal("expected quota error, got nil")
	}
	if !errors.Is(err, ErrStateDirQuotaExceeded) {
		t.Fatalf("expected ErrStateDirQuotaExceeded, got %v", err)
	}
	// The error must include the dir path so operators can act without
	// reverse-engineering the manager wiring.
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("error should embed the state dir path %q, got %q", dir, err.Error())
	}
}

// TestCheckStateDirQuota_AcceptsBelowLimit confirms the boundary on the
// permissive side: a state dir under quota passes, regardless of how many
// individual files it holds.
func TestCheckStateDirQuota_AcceptsBelowLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 4; i++ {
		path := filepath.Join(dir, "k"+string(rune('a'+i))+".json")
		if err := os.WriteFile(path, make([]byte, 256), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// 4 × 256 = 1024 B, under the 8 KiB cap.
	m := mustNewManager(t, ManagerConfig{
		StateDir:           dir,
		StateDirQuotaBytes: 8192,
	})
	if err := m.checkStateDirQuota(); err != nil {
		t.Fatalf("expected quota-below-limit pass, got %v", err)
	}
}

// TestCheckStateDirQuota_FailsOpenOnMissingDir confirms the gate is
// "fail-open" when the state dir cannot be scanned (first-run system,
// transient i/o failure). The shim spawn path must not break startup
// in this case — quota enforcement is best-effort.
func TestCheckStateDirQuota_FailsOpenOnMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	m := mustNewManager(t, ManagerConfig{
		StateDir:           missing,
		StateDirQuotaBytes: 1, // tiny quota would otherwise trip
	})
	// NewManager will MkdirAll the dir; remove it so the walk hits ENOENT.
	if err := os.RemoveAll(missing); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if err := m.checkStateDirQuota(); err != nil {
		t.Fatalf("missing state dir must fail-open, got %v", err)
	}
}

// TestStartShimWithBackend_QuotaErrorBeforeReservation confirms that the
// quota gate fires BEFORE the slot reservation: a rejected spawn must not
// leak m.pendingShims (otherwise repeated quota-blocked spawns would
// permanently shrink the available shim slot count). The test does not
// need to launch a real shim — the quota check sits at the very top of
// StartShimWithBackend, before the exec attempt.
func TestStartShimWithBackend_QuotaErrorBeforeReservation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fat.json"), make([]byte, 8192), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := mustNewManager(t, ManagerConfig{
		StateDir:           dir,
		StateDirQuotaBytes: 1024,
	})

	_, err := m.StartShimWithBackend(context.Background(), "k:p:1", "/bin/true", "", nil, dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrStateDirQuotaExceeded) {
		t.Fatalf("expected ErrStateDirQuotaExceeded, got %v", err)
	}

	// pendingShims must be 0 — the early-return path before the slot
	// reservation should leave m.pendingShims untouched.
	m.mu.Lock()
	pending := m.pendingShims
	m.mu.Unlock()
	if pending != 0 {
		t.Fatalf("quota-blocked spawn leaked pendingShims=%d (must stay 0)", pending)
	}
}
