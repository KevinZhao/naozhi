package backend

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestAll_NewProtocolReturnsNonNilProtocol pins the registry-wide contract
// that EVERY registered Profile produces a non-nil cli.Protocol from
// NewProtocol(ProtocolDeps{}). The existing tests
// (TestProfile_NewProtocol_Claude / _Kiro) assert this per hard-coded ID;
// this case asserts it across the whole registry so a new backend added to
// RegisterDefaults with a nil or panicking NewProtocol fails CI without
// anyone remembering to extend a per-ID test.
//
// This invariant is load-bearing for #1034 (R240-ARCH-20): that issue moves
// the backend package out from under internal/cli to break the
// cli/backend → cli reverse-import (Profile.NewProtocol returning
// cli.Protocol). After the move the return type contract — every profile
// constructs a usable cli.Protocol — must still hold, regardless of where
// the package lives. Locking it now means the refactor's green/red signal
// is unambiguous.
func TestAll_NewProtocolReturnsNonNilProtocol(t *testing.T) {
	withCleanRegistry(t, func() {
		RegisterDefaults()

		profiles := All()
		if len(profiles) == 0 {
			t.Fatal("RegisterDefaults produced an empty registry")
		}

		for _, p := range profiles {
			if p.NewProtocol == nil {
				t.Errorf("profile %q: NewProtocol is nil", p.ID)
				continue
			}
			// Must not panic and must return a non-nil cli.Protocol.
			var proto cli.Protocol = p.NewProtocol(ProtocolDeps{})
			if proto == nil {
				t.Errorf("profile %q: NewProtocol(ProtocolDeps{}) returned nil", p.ID)
				continue
			}
			// The protocol must self-identify with a non-empty Name so the
			// session layer can label it. A nil-but-typed interface (e.g. a
			// (*ClaudeProtocol)(nil)) would pass the != nil check above but
			// panic here — calling Name() proves the value is usable.
			if proto.Name() == "" {
				t.Errorf("profile %q: protocol Name() is empty", p.ID)
			}
		}
	})
}
