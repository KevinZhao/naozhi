package schema

import (
	"errors"
	"fmt"
	"testing"
)

// TestVersionHandshake_BoundaryInvariants pins the read-window contract that
// the R230B-ARCH-21 migration handshake relies on (ARCH6 / #386). Today
// MinReadVersion == WireVersion == 1, so the lower-boundary branch in
// UnmarshalRecord (`r.V < MinReadVersion`) is unreachable through the current
// constants. This test locks the invariants that make a future bump safe:
//
//   - 0 < MinReadVersion <= WireVersion (a read window can never be empty
//     or inverted; an empty window would refuse every file ever written).
//
// If a future change advances WireVersion (write the new format) while
// leaving MinReadVersion behind (still read the old format), this guard
// keeps the two boundaries from crossing — the failure mode the constants'
// godoc warns about.
func TestVersionHandshake_BoundaryInvariants(t *testing.T) {
	if MinReadVersion <= 0 {
		t.Fatalf("MinReadVersion=%d must be > 0", MinReadVersion)
	}
	if MinReadVersion > WireVersion {
		t.Fatalf("MinReadVersion=%d must be <= WireVersion=%d (empty/inverted read window)",
			MinReadVersion, WireVersion)
	}
}

// TestVersionHandshake_RejectsBelowMinRead pins the lower-boundary reject
// path (`r.V < MinReadVersion`) that ARCH6 / #386 flags as the migration
// seam. The branch is unreachable while MinReadVersion == 1, so the test
// drives it directly: any record whose declared version sits below the
// read window must surface ErrUnsupportedVersion (the same "we no longer
// guarantee back-compat" signal as a too-new version), NOT silently parse.
//
// This is the regression guard a later `MinReadVersion = 2` bump needs:
// without it, dropping the lower-boundary check would go unnoticed until a
// production reader best-effort-parsed a pre-migration file.
func TestVersionHandshake_RejectsBelowMinRead(t *testing.T) {
	if MinReadVersion <= 1 {
		// Synthesise a version strictly inside (0, MinReadVersion) only when
		// such a value exists. With MinReadVersion == 1 the lower-boundary
		// branch is genuinely unreachable (v<=0 is caught first as
		// ErrInvalidVersion), so we assert that documented behaviour instead.
		data := []byte(`{"v":0,"seq":1,"type":"entry","entry":{"time":1}}`)
		_, err := UnmarshalRecord(data)
		if !errors.Is(err, ErrInvalidVersion) {
			t.Fatalf("v=0 below window: err=%v, want ErrInvalidVersion", err)
		}
		return
	}

	// MinReadVersion has been bumped past 1: a positive version below the
	// window must be refused as unsupported.
	below := MinReadVersion - 1
	data := []byte(fmt.Sprintf(`{"v":%d,"seq":1,"type":"entry","entry":{"time":1}}`, below))
	_, err := UnmarshalRecord(data)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("v=%d below MinReadVersion=%d: err=%v, want ErrUnsupportedVersion",
			below, MinReadVersion, err)
	}
}

// TestVersionHandshake_AcceptsWireVersion is the positive companion: a record
// at exactly WireVersion (the only version writers ever emit today) must
// round-trip through UnmarshalRecord without a version error. Pairs with the
// reject tests so the accept window is pinned from both sides.
func TestVersionHandshake_AcceptsWireVersion(t *testing.T) {
	rec := NewEntry(1, []byte(`{"time":1}`))
	body, err := MarshalRecord(rec)
	if err != nil {
		t.Fatalf("MarshalRecord: %v", err)
	}
	got, err := UnmarshalRecord(body)
	if err != nil {
		t.Fatalf("UnmarshalRecord at WireVersion=%d: %v", WireVersion, err)
	}
	if got.V != WireVersion {
		t.Errorf("round-tripped V=%d, want WireVersion=%d", got.V, WireVersion)
	}
}
