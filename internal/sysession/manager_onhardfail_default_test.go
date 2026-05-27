package sysession

import (
	"os"
	"reflect"
	"testing"
)

// TestNewManager_DefaultOnHardFailBindsOSExitDirectly verifies the fix for
// #1287 (R20260527-GO-5): the default OnHardFail must point at os.Exit
// directly, not at a closure that reads the osExit package var at call
// time. If a Manager construction left cfg.OnHardFail unset and we later
// swap the osExit pkg var (e.g. for an unrelated test), the previously
// constructed Manager's default hook should NOT observe the swap — it
// should still call os.Exit.
//
// We assert by reflect.ValueOf().Pointer() comparison rather than by
// invoking the hook (which would terminate the test binary).
func TestNewManager_DefaultOnHardFailBindsOSExitDirectly(t *testing.T) {
	registryTestMu.Lock()
	defer registryTestMu.Unlock()

	router := newFakeRouter()
	m, err := NewManager(Config{
		Enabled: true,
		Router:  router,
		// OnHardFail intentionally left nil — exercise default branch.
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.cfg.OnHardFail == nil {
		t.Fatal("default OnHardFail was nil after NewManager")
	}
	want := reflect.ValueOf(os.Exit).Pointer()
	got := reflect.ValueOf(m.cfg.OnHardFail).Pointer()
	if got != want {
		t.Errorf("default OnHardFail does not point at os.Exit directly:\n got = %x\nwant = %x\n(closure capturing osExit pkg-var would yield a different pointer)", got, want)
	}
}

// TestNewManager_OsExitSwapDoesNotLeakIntoDefaultOnHardFail is the
// behavioural counterpart to the pointer assertion above. We swap the
// osExit pkg var to a marker function and confirm the default OnHardFail
// does NOT route through it. Calling OnHardFail with a non-fatal code
// would normally terminate the process; we therefore avoid invoking it
// at all and only rely on the identity check — but we keep the swap +
// restore pattern so a future regression that re-introduces the closure
// would be caught by the pointer comparison above.
func TestNewManager_OsExitSwapDoesNotLeakIntoDefaultOnHardFail(t *testing.T) {
	registryTestMu.Lock()
	defer registryTestMu.Unlock()

	prev := osExit
	osExit = func(int) { /* no-op marker; must NOT be reached */ }
	defer func() { osExit = prev }()

	router := newFakeRouter()
	m, err := NewManager(Config{Enabled: true, Router: router})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wantOSExit := reflect.ValueOf(os.Exit).Pointer()
	gotDefault := reflect.ValueOf(m.cfg.OnHardFail).Pointer()
	if gotDefault != wantOSExit {
		t.Errorf("default OnHardFail diverges from os.Exit after osExit swap: got=%x wantOSExit=%x", gotDefault, wantOSExit)
	}
	// And the swap must not equal default — sanity check that the pkg
	// var swap actually changed something the fix is supposed to ignore.
	gotSwap := reflect.ValueOf(osExit).Pointer()
	if gotSwap == gotDefault {
		t.Error("test setup error: osExit swap landed on the same address as os.Exit, fix is untested")
	}
}
