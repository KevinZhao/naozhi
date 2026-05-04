package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
func TestManagedSession_AtomicPointerStringFields(t *testing.T) {
	t.Parallel()
	want := reflect.TypeOf(atomic.Pointer[string]{})
	typ := reflect.TypeOf(ManagedSession{})
	for _, name := range []string{
		"sessionID", "lastPrompt", "lastActivity",
		"backend", "cliName", "cliVersion", "deathReason", "userLabel",
		"workspace",
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
}

// TestSessionPackage_NoLegacyAtomicValueForStrings walks session + cli source
// files looking for `atomic.Value` in struct field declarations to ensure the
// migration stays clean. The check is conservative: it parses each .go file
// and inspects struct types directly so `atomic.Value` mentioned in a comment
// (documenting the migration itself) does not trigger a false positive.
func TestSessionPackage_NoLegacyAtomicValueForStrings(t *testing.T) {
	t.Parallel()
	roots := []string{
		filepath.Join("..", "session"),
		filepath.Join("..", "cli"),
	}
	violations := []string{}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			// Skip tests — they may legitimately mention atomic.Value in
			// migration/contract documentation.
			if strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, e.Name())
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				st, ok := n.(*ast.StructType)
				if !ok || st.Fields == nil {
					return true
				}
				for _, field := range st.Fields.List {
					sel, ok := field.Type.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					pkg, ok := sel.X.(*ast.Ident)
					if !ok {
						continue
					}
					if pkg.Name == "atomic" && sel.Sel.Name == "Value" {
						names := []string{}
						for _, n := range field.Names {
							names = append(names, n.Name)
						}
						violations = append(violations,
							path+": atomic.Value field(s) "+strings.Join(names, ",")+
								" — migrate to atomic.Pointer[T] for type safety")
					}
				}
				return true
			})
		}
	}
	if len(violations) > 0 {
		t.Errorf("found %d atomic.Value field declaration(s):\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestLoadStringAtomic_NilPointer_ReturnsEmpty locks the helper's zero-value
// contract: an untouched atomic.Pointer[string] has Load()==nil, and
// loadStringAtomic must collapse that to "" (Snapshot / Backend / CLIName all
// rely on this semantic to avoid nil-deref).
func TestLoadStringAtomic_NilPointer_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	if got := loadStringAtomic(&v); got != "" {
		t.Errorf("loadStringAtomic(nil ptr) = %q, want \"\"", got)
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
	storeStringAtomic(&v, "hot-tool-label")
	firstPtr := v.Load()
	if firstPtr == nil || *firstPtr != "hot-tool-label" {
		t.Fatalf("first store failed: got %v", firstPtr)
	}
	// Second store with the same value MUST be a no-op: pointer identity unchanged.
	storeStringAtomic(&v, "hot-tool-label")
	if got := v.Load(); got != firstPtr {
		t.Errorf("equal-value second store allocated a new pointer (%p != %p) — fast path regression", got, firstPtr)
	}
	// Divergent store MUST write a fresh pointer.
	storeStringAtomic(&v, "different-label")
	if got := v.Load(); got == firstPtr {
		t.Errorf("divergent store skipped write — compare-before-store semantics broken")
	}
	if got := loadStringAtomic(&v); got != "different-label" {
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
	storeStringAtomic(&v, "")
	if v.Load() == nil {
		t.Error("first store of \"\" from nil pointer was skipped — fast path must not short-circuit when cur==nil")
	}
}

// TestStoreStringAtomic_RoundTrip covers the common mutation path: store a
// string, read it back via loadStringAtomic. A naive `v.Store(&s)` where `s`
// is a loop-captured variable would let later writes mutate prior pointers;
// the helper's by-value argument makes each call self-contained.
func TestStoreStringAtomic_RoundTrip(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	storeStringAtomic(&v, "first")
	if got := loadStringAtomic(&v); got != "first" {
		t.Errorf("after first store: got %q, want \"first\"", got)
	}
	storeStringAtomic(&v, "second")
	if got := loadStringAtomic(&v); got != "second" {
		t.Errorf("after second store: got %q, want \"second\"", got)
	}
	// Explicit empty store: the field pointer is non-nil but the payload is
	// "". This is distinct from "never stored" (nil) — a future caller that
	// cares to distinguish the two can do so via v.Load() directly.
	storeStringAtomic(&v, "")
	if got := loadStringAtomic(&v); got != "" {
		t.Errorf("after empty store: got %q, want \"\"", got)
	}
	if v.Load() == nil {
		t.Error("explicit empty store must leave pointer non-nil (so CAS paths can distinguish \"set\" from \"unset\")")
	}
}
