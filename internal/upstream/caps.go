// upstream/caps.go: derive the reverse-node Capabilities slice that
// accompanies the register frame.
//
// Sprint 6b of docs/rfc/multi-backend.md spec'd this auto-derivation
// so that an operator running `naozhi` as a sub-node behind a primary
// no longer needs to maintain a hand-curated `upstream.capabilities`
// list parallel to the backend.Profile registry. Adding kiro / gemini
// to RegisterDefaults at compile time automatically widens what this
// node advertises.
//
// The output is deterministic (sort.Strings) so packet captures and
// primary-side logs match across reconnects — useful for diffing
// "what did this node advertise yesterday vs now" in incident triage.
package upstream

import (
	"sort"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// derivedCaps walks every registered backend.Profile and returns the
// sorted union of RequiredNodeCaps. Empty when:
//   - no profiles are registered (test paths that skip
//     backend.RegisterDefaults), or
//   - every registered profile has nil/empty RequiredNodeCaps (the
//     legacy claude-only build).
//
// The empty-result branch returns nil rather than []string{} so the
// resulting ReverseMsg omits the Capabilities field via omitempty —
// preserving wire compatibility with primaries on a naozhi version
// that predates capability negotiation.
func derivedCaps() []string {
	var seen map[string]struct{}
	for _, p := range backend.All() {
		for _, c := range p.RequiredNodeCaps {
			if c == "" {
				continue
			}
			if seen == nil {
				seen = make(map[string]struct{}, 4)
			}
			seen[c] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
