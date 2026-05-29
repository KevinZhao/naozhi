package session

import (
	"math"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Workspace returns the effective cwd recorded for this session. Lock-free;
// safe to call from Hub handlers and other call sites that don't hold r.mu.
func (s *ManagedSession) Workspace() string { return loadAtomicString(&s.workspace) }

// setWorkspace stores the workspace path atomically. Router-internal helper —
// all writers already hold r.mu, but we route through the helper so the string
// is always handed to the atomic.Pointer via one place (matches
// storeAtomicString convention for backend/cliName/cliVersion).
func (s *ManagedSession) setWorkspace(ws string) { storeAtomicString(&s.workspace, ws) }

// IsExempt returns whether this session is exempt from TTL and eviction.
func (s *ManagedSession) IsExempt() bool { return s.exempt }

// loadAtomicString and storeAtomicString are thin wrappers around the shared
// textutil.LoadAtomicString / textutil.StoreAtomicString helpers. Kept as
// package-private aliases so the surrounding accessor methods read cleanly.
// Behavioural contract — fast-path short-circuit on equal value,
// last-writer-wins — is documented on the textutil helpers; do not
// re-document it here to keep the two in sync.
//
// Naming follows the textutil canonical (action-type) so cli and session
// thin wrappers no longer have inverted word order. R219-CR-1 closed the
// duplication; this rename closes the naming inconsistency.
func loadAtomicString(v *atomic.Pointer[string]) string {
	return textutil.LoadAtomicString(v)
}

func storeAtomicString(v *atomic.Pointer[string], s string) {
	textutil.StoreAtomicString(v, s)
}

// loadTotalCost reads the float64 cumulative cost from an atomic.Uint64
// field, decoding the IEEE-754 bit pattern via math.Float64frombits.
// Returns 0 when the field has never been written (Load() → 0 maps to
// float64 zero, same default the plain-float64 field had).
//
// Cross-ref: textutil exposes LoadAtomicString / StoreAtomicString for the
// `atomic.Pointer[string]` mirror pattern (R219-CR-1) but does not yet
// cover the `atomic.Uint64`-encoded float64 case used here. These helpers
// stay package-local until a second call site emerges; lifting them into
// textutil now would invert the dependency (textutil is a leaf package
// that must not import session-specific contracts). R230-CQ-18.
func loadTotalCost(v *atomic.Uint64) float64 {
	return math.Float64frombits(v.Load())
}

// storeTotalCost writes a float64 cumulative cost via atomic.Uint64,
// encoding through math.Float64bits. Paired with loadTotalCost to keep the
// packing/unpacking convention in one place — R183-CONCUR-M2 made the
// field atomic to harden against future post-publication writers, and
// having a helper keeps call sites free of bit-level noise.
//
// See loadTotalCost for the textutil cross-reference.
func storeTotalCost(v *atomic.Uint64, cost float64) {
	v.Store(math.Float64bits(cost))
}

// cliIdentityBox is the immutable triple stored in ManagedSession.cliIdentity.
// Treated as a value type — every update swaps the whole pointer rather than
// mutating fields in place, so readers always observe a consistent snapshot
// (no torn cliName-vs-cliVersion read across a partial write).
type cliIdentityBox struct {
	backend    string // "" = router default
	cliName    string // e.g. "claude-code", "kiro"
	cliVersion string // semver from --version
}

// loadCLIIdentity returns a copy of the current backend/cliName/cliVersion
// triple in one atomic Load. Returns the zero box (all fields "") when
// the session was constructed bare and nothing has been set yet — callers
// must treat that as "use router default", same as the legacy nil-pointer
// path did.
func (s *ManagedSession) loadCLIIdentity() cliIdentityBox {
	if box := s.cliIdentity.Load(); box != nil {
		return *box
	}
	return cliIdentityBox{}
}

// updateCLIIdentity is the CAS-loop primitive that all Set* helpers below
// funnel through. mut takes the current box (zero value when unset) and
// returns the desired next box; we retry until the CAS succeeds. This
// keeps independent SetBackend / SetCLIName / SetCLIVersion calls
// composable — concurrent writers from spawn / reconnect under r.mu plus
// occasional shim-discovery writes don't drop fields. The fast path
// short-circuits when mut returns an unchanged box, mirroring the
// loadAtomicString convention (see textutil.LoadAtomicString docs).
func (s *ManagedSession) updateCLIIdentity(mut func(cliIdentityBox) cliIdentityBox) {
	for {
		cur := s.cliIdentity.Load()
		var curVal cliIdentityBox
		if cur != nil {
			curVal = *cur
		}
		next := mut(curVal)
		if cur != nil && next == *cur {
			return
		}
		nextCopy := next
		if s.cliIdentity.CompareAndSwap(cur, &nextCopy) {
			return
		}
	}
}

// Backend returns the backend ID ("" when the router default is in effect).
func (s *ManagedSession) Backend() string { return s.loadCLIIdentity().backend }

// SetBackend records the backend ID for this session. Called at spawn time
// and (rarely) by reconnectShims after a naozhi restart.
func (s *ManagedSession) SetBackend(id string) {
	s.updateCLIIdentity(func(cur cliIdentityBox) cliIdentityBox {
		cur.backend = id
		return cur
	})
}

// CLIName returns the CLI display name (e.g. "claude-code", "kiro").
func (s *ManagedSession) CLIName() string { return s.loadCLIIdentity().cliName }

// SetCLIName records the wrapper-provided CLI display name.
func (s *ManagedSession) SetCLIName(name string) {
	s.updateCLIIdentity(func(cur cliIdentityBox) cliIdentityBox {
		cur.cliName = name
		return cur
	})
}

// CLIVersion returns the detected CLI version string.
func (s *ManagedSession) CLIVersion() string { return s.loadCLIIdentity().cliVersion }

// SetCLIVersion records the wrapper-provided CLI version.
func (s *ManagedSession) SetCLIVersion(v string) {
	s.updateCLIIdentity(func(cur cliIdentityBox) cliIdentityBox {
		cur.cliVersion = v
		return cur
	})
}

// UserLabel returns the operator-set display label ("" when unset).
func (s *ManagedSession) UserLabel() string { return loadAtomicString(&s.userLabel) }

// SetUserLabel records an operator-set display label. Callers must have
// already validated length/charset; the empty string clears any prior label.
//
// Deprecated for daemon callers: prefer Router.SetUserLabelWithOrigin so the
// LabelOrigin field stays consistent. This bare setter is preserved for
// internal callers (router restore, tests) that already know the origin
// context they want to preserve.
func (s *ManagedSession) SetUserLabel(v string) { storeAtomicString(&s.userLabel, v) }

// LabelOrigin returns the recorded origin of the current UserLabel:
// "" (legacy / empty equivalent to "user") / "user" / "auto". Lock-free.
func (s *ManagedSession) LabelOrigin() string { return loadAtomicString(&s.labelOrigin) }

// setLabelOrigin records the origin of the current UserLabel. Unexported
// because the only legitimate writers are Router.SetUserLabelWithOrigin
// and ClearUserLabelOrigin, which run under r.mu so the re-read protocol
// (RFC §11.1) stays atomic with the userLabel update.
func (s *ManagedSession) setLabelOrigin(v string) { storeAtomicString(&s.labelOrigin, v) }

// Model returns the persisted last-known CLI model identifier ("" when
// not yet captured from system/init / SpawnOptions). UI Round 5 R5-3.
func (s *ManagedSession) Model() string { return loadAtomicString(&s.model) }

// SetModel records the latest known model id. Called by the readLoop
// snapshotter when proc.Model() flips from "" to a real value, AND by
// the store-restore path in NewRouter when seeding from sessions.json.
func (s *ManagedSession) SetModel(v string) { storeAtomicString(&s.model, v) }

// SetHistorySource installs the backend-specific disk-tier Source. Called
// by the router at session construction; safe to call after the session is
// published (atomic store) but callers should not rely on mid-flight
// swaps being observed by a pagination request already in progress.
// nil disables disk fallback (equivalent to history.Noop).
func (s *ManagedSession) SetHistorySource(src history.Source) {
	s.historySource.Store(&historySourceBox{src: src})
}

// loadHistorySource returns the installed Source, or nil when no source
// has been attached yet. Callers treat nil the same as history.Noop.
func (s *ManagedSession) loadHistorySource() history.Source {
	box := s.historySource.Load()
	if box == nil {
		return nil
	}
	return box.src
}
