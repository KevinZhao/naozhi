package upstream

import (
	"reflect"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
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
// has no caps, kiro has "acp", codex has "codex-app-server"; the wire
// output adds the two always-advertised tags, alpha-sorted.
//
// If we ever add a backend with a cap, the assertion will fail
// loudly — by design — so the operator-facing register frame change
// is reviewed deliberately.
func TestDerivedCaps_FromDefaultRegistry(t *testing.T) {
	seedDefaultBackends(t)

	got := derivedCaps()
	want := []string{"acp", "codex-app-server", clievent.SchemaCap, node.CapSubscribeHistory}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("derivedCaps() = %v; want %v", got, want)
	}
}

// TestDerivedCaps_DeterministicSort ensures registration order does
// not leak into the output: a profile with a cap that sorts before "acp",
// listed last, still comes out first. It builds its own profile list rather
// than registering into the package registry, which has no unregister — a
// synthetic profile left there would change what
// TestDerivedCaps_FromDefaultRegistry sees on every later run.
func TestDerivedCaps_DeterministicSort(t *testing.T) {
	seedDefaultBackends(t)
	profiles := append(backend.All(), backend.Profile{
		ID:               "synth-aaa",
		RequiredNodeCaps: []string{"aaa-cap"},
	})

	got := capsOf(profiles)
	if len(got) < 2 {
		t.Fatalf("expected ≥2 caps, got %v", got)
	}
	if got[0] != "aaa-cap" {
		t.Errorf("expected sorted output (aaa-cap first), got %v", got)
	}
}
