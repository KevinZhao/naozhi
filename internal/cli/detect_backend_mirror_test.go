package cli_test

// Drift guard for the cli-side backend mirror — R0530-ARCH-3 (related: #408).
//
// internal/cli/detect.go keeps two hand-maintained tables, knownBackends and
// knownBackendBinaries, that mirror the authoritative backend.Profile registry
// in internal/cli/backend (backend.All()). The mirror exists only because the
// backend package imports cli, so cli cannot import backend back without an
// import cycle; until that cycle is broken (OPEN issue #408, needs-design — a
// dedicated backendreg subpackage that both cli and backend can import), the
// cli side must restate ID + DefaultBinary by hand.
//
// Hand mirrors rot silently: adding a backend means editing three places
// (a profile_<id>.go, knownBackends, knownBackendBinaries) with no compile-time
// link between them. A missed edit degrades at runtime (the new backend is
// simply never probed) instead of failing the build. This test turns any such
// drift into a CI failure by asserting the cli mirror's ID/binary sets are
// exactly the backend.Profile registry's sets.
//
// This test lives in package cli_test (not package cli) precisely so it can
// import internal/cli/backend without the cycle; it reaches knownBackends /
// knownBackendBinaries through the test-only export bridge in
// detect_backend_mirror_export_test.go. Delete both files once #408 lands and
// the mirror is gone.

import (
	"sort"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
)

func TestBackendMirrorMatchesRegistry(t *testing.T) {
	// EnsureDefaults is idempotent + concurrent-safe, so it is safe to call
	// from a test even if main / another test already registered the
	// built-in profiles.
	backend.EnsureDefaults()

	profiles := backend.All()
	if len(profiles) == 0 {
		t.Fatal("backend.All() returned no profiles after EnsureDefaults(); registry is empty")
	}

	// Authoritative source: backend.Profile registry.
	wantIDs := make([]string, 0, len(profiles))
	wantBinaries := make(map[string]string, len(profiles))
	for _, p := range profiles {
		wantIDs = append(wantIDs, p.ID)
		wantBinaries[p.ID] = p.DefaultBinary
	}

	// cli-side mirror: knownBackends (IDs) + knownBackendBinaries (binaries).
	mirror := cli.ExportedKnownBackends
	gotIDs := make([]string, 0, len(mirror))
	for _, b := range mirror {
		gotIDs = append(gotIDs, b.ID)
	}
	gotBinaries := cli.ExportedKnownBackendBinaries

	// 1) ID set parity: knownBackends must enumerate exactly the registry IDs.
	if !sameStringSet(gotIDs, wantIDs) {
		t.Errorf("knownBackends ID set drifted from backend.All():\n  knownBackends = %v\n  backend.All() = %v\nAdd/remove the backend in internal/cli/detect.go to match the Profile registry (R0530-ARCH-3, #408).",
			sortedCopy(gotIDs), sortedCopy(wantIDs))
	}

	// 2) Binary-key set parity: knownBackendBinaries must key on exactly the
	//    registry IDs too.
	gotBinaryKeys := make([]string, 0, len(gotBinaries))
	for id := range gotBinaries {
		gotBinaryKeys = append(gotBinaryKeys, id)
	}
	if !sameStringSet(gotBinaryKeys, wantIDs) {
		t.Errorf("knownBackendBinaries key set drifted from backend.All():\n  knownBackendBinaries keys = %v\n  backend.All() IDs         = %v\nAdd/remove the entry in internal/cli/detect.go (R0530-ARCH-3, #408).",
			sortedCopy(gotBinaryKeys), sortedCopy(wantIDs))
	}

	// 3) DefaultBinary value parity: for each registry ID the cli mirror's
	//    default binary must equal Profile.DefaultBinary.
	for id, wantBin := range wantBinaries {
		gotBin, ok := gotBinaries[id]
		if !ok {
			// Already reported by the key-set check above; skip to avoid noise.
			continue
		}
		if gotBin != wantBin {
			t.Errorf("knownBackendBinaries[%q] = %q, want %q (backend.Profile.DefaultBinary). Update internal/cli/detect.go (R0530-ARCH-3, #408).",
				id, gotBin, wantBin)
		}
	}
}

// sameStringSet reports whether a and b contain the same elements, ignoring
// order and treating each as a set (duplicates collapse).
func sameStringSet(a, b []string) bool {
	sa := make(map[string]struct{}, len(a))
	for _, s := range a {
		sa[s] = struct{}{}
	}
	sb := make(map[string]struct{}, len(b))
	for _, s := range b {
		sb[s] = struct{}{}
	}
	if len(sa) != len(sb) {
		return false
	}
	for s := range sa {
		if _, ok := sb[s]; !ok {
			return false
		}
	}
	return true
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
