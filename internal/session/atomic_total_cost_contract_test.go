package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// TestManagedSession_TotalCostIsAtomicUint64 locks the R183-CONCUR-M2
// migration: ManagedSession.totalCost moved from a plain float64 (torn-read
// prone if any future refactor adds a post-publication writer) to an
// atomic.Uint64 holding a math.Float64bits pack. Reverting the type drops the
// type-level guarantee and reopens the race surface — this test makes that
// an immediate compile-time / test-time failure.
func TestManagedSession_TotalCostIsAtomicUint64(t *testing.T) {
	t.Parallel()
	want := reflect.TypeOf(atomic.Uint64{})
	typ := reflect.TypeOf(ManagedSession{})
	f, ok := typ.FieldByName("totalCost")
	if !ok {
		t.Fatal("ManagedSession.totalCost missing (renamed?)")
	}
	if f.Type != want {
		t.Errorf("ManagedSession.totalCost type = %v, want atomic.Uint64 — "+
			"R183-CONCUR-M2 hardened the field against torn reads; do not revert to plain float64",
			f.Type)
	}
}

// TestLoadStoreTotalCost_RoundTrip pins the helper contract: storeTotalCost
// encodes via math.Float64bits, loadTotalCost decodes via math.Float64frombits,
// and the composition is an identity for all representable float64 values
// (including NaN bit patterns, which are handled via Float64bits equality even
// though NaN != NaN in ==).
func TestLoadStoreTotalCost_RoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   float64
	}{
		{"zero", 0.0},
		{"positive small", 0.0001},
		{"positive large", 1234567.89},
		{"one cent", 0.01},
		{"denormal", math.SmallestNonzeroFloat64},
		{"max", math.MaxFloat64},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var v atomic.Uint64
			storeTotalCost(&v, tc.in)
			if got := loadTotalCost(&v); got != tc.in {
				t.Errorf("round-trip: storeTotalCost(%v) -> loadTotalCost = %v", tc.in, got)
			}
		})
	}
}

// TestLoadTotalCost_UntouchedIsZero locks the zero-value semantic: a never-
// written atomic.Uint64 has Load() == 0, and math.Float64frombits(0) == 0.0
// (IEEE-754 positive zero). Snapshot's "no process" branch relies on this
// to surface $0.00 for never-billed sessions rather than a nonsense NaN.
func TestLoadTotalCost_UntouchedIsZero(t *testing.T) {
	t.Parallel()
	var v atomic.Uint64
	if got := loadTotalCost(&v); got != 0 {
		t.Errorf("loadTotalCost on untouched field = %v, want 0", got)
	}
}

// TestTotalCost_ConcurrentReadWriteNoTornRead is the headline R183-CONCUR-M2
// regression test. On 32-bit platforms a plain float64 write is two machine
// instructions; a reader could observe a torn value composed of half the old
// write and half the new. The atomic.Uint64 + math.Float64bits pack eliminates
// that window. We stress with one writer alternating between two very
// distinct bit patterns and N readers asserting every read equals one of the
// two sentinel values — anything else (including NaN from a torn read) fails
// the assertion.
//
// The loop is sized so even slow sanitizer builds complete in a handful of
// milliseconds while still exercising the read/write interleaving enough to
// catch an obvious regression.
func TestTotalCost_ConcurrentReadWriteNoTornRead(t *testing.T) {
	t.Parallel()
	var v atomic.Uint64
	// Two distinct double-precision values with non-overlapping high/low 32-bit
	// halves so any torn read on a 32-bit-word machine would produce a value
	// that matches neither sentinel.
	const a = 1.2345678901234567e200
	const b = 9.8765432109876543e-200
	// Seed with a so readers always have a defined value to observe.
	storeTotalCost(&v, a)

	const iters = 20000
	const readers = 4
	done := make(chan struct{})
	var failCount atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				got := loadTotalCost(&v)
				if got != a && got != b {
					failCount.Add(1)
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if i&1 == 0 {
				storeTotalCost(&v, a)
			} else {
				storeTotalCost(&v, b)
			}
			if i%1024 == 0 {
				runtime.Gosched()
			}
		}
		close(done)
	}()

	wg.Wait()
	if n := failCount.Load(); n > 0 {
		t.Errorf("observed %d torn reads — atomic packing broken", n)
	}
}

// TestSessionPackage_NoPlainFloat64TotalCost scans session/*.go for a struct
// field named totalCost whose declared type is plain float64. If a future
// refactor reverts the R183-CONCUR-M2 atomic wrapping (say by introducing a
// helper struct that holds the float directly), this guards against silent
// regression. Complement to the reflect-based test above: catches renames and
// structural splits that would slip past the type check on ManagedSession.
func TestSessionPackage_NoPlainFloat64TotalCost(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "session")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	violations := []string{}
	for _, e := range entries {
		if e.IsDir() || !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if !hasSuffix(name, ".go") || hasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(root, name)
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
				ident, ok := field.Type.(*ast.Ident)
				if !ok || ident.Name != "float64" {
					continue
				}
				for _, n := range field.Names {
					if n.Name == "totalCost" {
						violations = append(violations,
							path+": totalCost declared as plain float64 — "+
								"R183-CONCUR-M2 requires atomic.Uint64 + loadTotalCost/storeTotalCost helpers")
					}
				}
			}
			return true
		})
	}
	if len(violations) > 0 {
		t.Errorf("found %d plain-float64 totalCost declaration(s):\n  %s",
			len(violations), joinLines(violations))
	}
}

func hasSuffix(s, suf string) bool {
	if len(s) < len(suf) {
		return false
	}
	return s[len(s)-len(suf):] == suf
}

func joinLines(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "\n  "
		}
		out += x
	}
	return out
}
