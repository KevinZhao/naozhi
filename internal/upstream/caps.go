// upstream/caps.go: derive the reverse-node Capabilities slice that
// accompanies the register frame. Auto-derived from the backend.Profile
// registry (docs/rfc/multi-backend.md) so a sub-node needs no hand-curated
// `upstream.capabilities` list; output is sorted for deterministic wire output.
package upstream

import (
	"sort"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// derivedCaps returns the sorted union of RequiredNodeCaps across every
// registered backend.Profile. Returns nil (not []string{}) when empty so the
// ReverseMsg omits Capabilities via omitempty, keeping wire compatibility
// with primaries that predate capability negotiation.
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
	if seen == nil {
		seen = make(map[string]struct{}, 1)
	}
	// Always advertised: the EventEntry schema this binary speaks, so a
	// mixed-version primary can gate a future semantic change (#2496).
	seen[clievent.SchemaCap] = struct{}{}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
