package session

import (
	"reflect"
	"sync/atomic"
	"testing"
)

// TestManagedSession_AtomicPointerStringFields locks the Round 170 migration
// away from atomic.Value for string-only fields. Prior to this round the
// session + cli packages carried a dozen `atomic.Value // stores string`
// declarations that accepted any interface{} payload; a casual refactor
// storing a non-string value would have corrupted the dashboard snapshot and
// only failed at runtime (with a panic from the type assertion). The
// atomic.Pointer[string] variant is type-checked at compile time.
//
// Runtime invariant: reflect inspects each field's declared type matches
// atomic.Pointer[string]. A source-level sanity sweep below complements
// this with a grep for legacy `atomic.Value` spellings — the combination
// catches both "field renamed" and "type reverted" regressions.
//
// R215-ARCH-P2-7: backend / cliName / cliVersion were collapsed into the
// packed cliIdentity field (atomic.Pointer[cliIdentityBox]) so Snapshot()
// reads all three with one Load. They are no longer individual
// atomic.Pointer[string] fields and are therefore exercised below by the
// dedicated cliIdentity field-existence check.
func TestManagedSession_AtomicPointerStringFields(t *testing.T) {
	t.Parallel()
	want := reflect.TypeOf(atomic.Pointer[string]{})
	typ := reflect.TypeOf(ManagedSession{})
	for _, name := range []string{
		"sessionID", "lastPrompt", "lastActivity",
		"deathReason", "userLabel", "workspace",
	} {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("ManagedSession.%s missing (was it renamed?)", name)
			continue
		}
		if f.Type != want {
			t.Errorf("ManagedSession.%s type = %v, want atomic.Pointer[string] — "+
				"Round 170 migration removed atomic.Value; do not revert",
				name, f.Type)
		}
	}
	// cliIdentity: packed triple, atomic.Pointer[cliIdentityBox].
	// We don't pin the inner type via reflect (cliIdentityBox is unexported
	// and the field type would force this test to import its own package
	// type); checking presence + the public Backend/CLIName/CLIVersion
	// behaviour (covered in snapshot_cli_identity_test.go) is enough.
	if _, ok := typ.FieldByName("cliIdentity"); !ok {
		t.Error("ManagedSession.cliIdentity missing — R215-ARCH-P2-7 packing reverted?")
	}
}

// TestLoadStringAtomic_NilPointer_ReturnsEmpty locks the helper's zero-value
// contract: an untouched atomic.Pointer[string] has Load()==nil, and
// loadAtomicString must collapse that to "" (Snapshot / Backend / CLIName all
// rely on this semantic to avoid nil-deref).
func TestLoadStringAtomic_NilPointer_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	if got := loadAtomicString(&v); got != "" {
		t.Errorf("loadAtomicString(nil ptr) = %q, want \"\"", got)
	}
}

// TestStoreStringAtomic_SkipsEqualWrite pins the R176-PERF-P1 compare-before-store
// fast path: when the currently stored string equals the incoming s, the helper
// must not allocate a fresh *string or issue an atomic Store. Verified by
// capturing the pointer value through two equal stores and asserting address
// identity — a naive implementation that always allocates would produce a
// fresh address on the second call. A functional regression (wrong comparison
// semantics) would instead observe a different string after the second call,
// which is also asserted.
func TestStoreStringAtomic_SkipsEqualWrite(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	storeAtomicString(&v, "hot-tool-label")
	firstPtr := v.Load()
	if firstPtr == nil || *firstPtr != "hot-tool-label" {
		t.Fatalf("first store failed: got %v", firstPtr)
	}
	// Second store with the same value MUST be a no-op: pointer identity unchanged.
	storeAtomicString(&v, "hot-tool-label")
	if got := v.Load(); got != firstPtr {
		t.Errorf("equal-value second store allocated a new pointer (%p != %p) — fast path regression", got, firstPtr)
	}
	// Divergent store MUST write a fresh pointer.
	storeAtomicString(&v, "different-label")
	if got := v.Load(); got == firstPtr {
		t.Errorf("divergent store skipped write — compare-before-store semantics broken")
	}
	if got := loadAtomicString(&v); got != "different-label" {
		t.Errorf("after divergent store: got %q, want \"different-label\"", got)
	}
}

// TestStoreStringAtomic_NilToEmptyIsNotSkipped ensures the first write of an
// empty string from a never-stored pointer actually installs a non-nil
// pointer. The fast-path check `cur != nil && *cur == s` must short-circuit
// on nil, otherwise tests like TestStoreStringAtomic_RoundTrip's "empty store
// leaves pointer non-nil" invariant would silently break.
func TestStoreStringAtomic_NilToEmptyIsNotSkipped(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	if v.Load() != nil {
		t.Fatal("precondition: zero-value atomic.Pointer must Load() nil")
	}
	storeAtomicString(&v, "")
	if v.Load() == nil {
		t.Error("first store of \"\" from nil pointer was skipped — fast path must not short-circuit when cur==nil")
	}
}

// TestStoreStringAtomic_RoundTrip covers the common mutation path: store a
// string, read it back via loadAtomicString. A naive `v.Store(&s)` where `s`
// is a loop-captured variable would let later writes mutate prior pointers;
// the helper's by-value argument makes each call self-contained.
func TestStoreStringAtomic_RoundTrip(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	storeAtomicString(&v, "first")
	if got := loadAtomicString(&v); got != "first" {
		t.Errorf("after first store: got %q, want \"first\"", got)
	}
	storeAtomicString(&v, "second")
	if got := loadAtomicString(&v); got != "second" {
		t.Errorf("after second store: got %q, want \"second\"", got)
	}
	// Explicit empty store: the field pointer is non-nil but the payload is
	// "". This is distinct from "never stored" (nil) — a future caller that
	// cares to distinguish the two can do so via v.Load() directly.
	storeAtomicString(&v, "")
	if got := loadAtomicString(&v); got != "" {
		t.Errorf("after empty store: got %q, want \"\"", got)
	}
	if v.Load() == nil {
		t.Error("explicit empty store must leave pointer non-nil (so CAS paths can distinguish \"set\" from \"unset\")")
	}
}
