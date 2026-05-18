package upstream

import (
	"reflect"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
)

// withDefaultBackendsForTest seeds backend.RegisterDefaults exactly
// once per test process. Mirrors the helper in package server so the
// upstream package can exercise derivedCaps without each test
// fighting the duplicate-registration panic in backend.Register.
var withDefaultBackendsForTest sync.Once

func seedDefaultBackends(t *testing.T) {
	t.Helper()
	withDefaultBackendsForTest.Do(func() {
		if len(backend.All()) == 0 {
			backend.RegisterDefaults()
		}
	})
}

// TestDerivedCaps_FromDefaultRegistry asserts that the union over the
// shipped Profiles produces the expected sorted slice. Today: claude
// has no caps, kiro has "acp"; the wire output is ["acp"].
//
// If we ever add a backend with a cap, the assertion will fail
// loudly — by design — so the operator-facing register frame change
// is reviewed deliberately.
func TestDerivedCaps_FromDefaultRegistry(t *testing.T) {
	seedDefaultBackends(t)

	got := derivedCaps()
	want := []string{"acp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("derivedCaps() = %v; want %v", got, want)
	}
}

// TestDerivedCaps_DeterministicSort ensures registration order does
// not leak into the output. Register a synthetic profile with a cap
// that sorts before "acp" and assert the result is alpha-sorted.
//
// We can't safely register and unregister within a test (registry
// reset is unexported); use a package-level sync.Once + a unique id
// so the synthetic profile is added once and other tests still see
// it deterministically.
var deterministicSortOnce sync.Once

func TestDerivedCaps_DeterministicSort(t *testing.T) {
	seedDefaultBackends(t)
	deterministicSortOnce.Do(func() {
		backend.Register(backend.Profile{
			ID:               "synth-aaa",
			DisplayName:      "synth-aaa",
			DefaultBinary:    "synth-aaa",
			DefaultTag:       "syn",
			NewProtocol:      func(_ backend.ProtocolDeps) cli.Protocol { return nil },
			DetectInProc:     func(_ string) bool { return false },
			RequiredNodeCaps: []string{"aaa-cap"},
		})
	})

	got := derivedCaps()
	if len(got) < 2 {
		t.Fatalf("expected ≥2 caps, got %v", got)
	}
	if got[0] != "aaa-cap" {
		t.Errorf("expected sorted output (aaa-cap first), got %v", got)
	}
}
