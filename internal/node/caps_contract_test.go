package node

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// TestKnownServerCaps_CoversBackendRequiredCaps pins R202606f-ARCH-4 (#2302).
//
// Two cap lists are derived independently and only kept consistent by comment:
//   - internal/upstream/caps.go derivedCaps() advertises the union of every
//     backend.Profile.RequiredNodeCaps in the register frame, and
//   - internal/node/caps.go knownServerCaps is the recognition set; a remote
//     advertising a cap outside it triggers a per-register "advertised unknown
//     capabilities" WARN.
//
// Today only "acp" (kiro) and "codex-app-server" (codex) flow out of
// RequiredNodeCaps and both happen to be in knownServerCaps, so the drift is
// latent. But a future backend that adds a RequiredNodeCap and forgets to
// extend knownServerCaps would make every same-version node permanently
// warn against itself. This test makes that omission a build/CI failure
// instead of a runtime log line.
//
// Contract: { c | p ∈ backend.All(), c ∈ p.RequiredNodeCaps } ⊆ knownServerCaps.
func TestKnownServerCaps_CoversBackendRequiredCaps(t *testing.T) {
	backend.EnsureDefaults()

	profiles := backend.All()
	if len(profiles) == 0 {
		t.Fatal("no backend profiles registered; EnsureDefaults did not populate the registry")
	}

	for _, p := range profiles {
		for _, c := range p.RequiredNodeCaps {
			if c == "" {
				continue
			}
			if _, ok := knownServerCaps[c]; !ok {
				t.Errorf("backend %q requires node cap %q which is NOT in node.knownServerCaps; "+
					"a same-version node would self-warn on every register. "+
					"Add %q to knownServerCaps in internal/node/caps.go.", p.ID, c, c)
			}
		}
	}
}
