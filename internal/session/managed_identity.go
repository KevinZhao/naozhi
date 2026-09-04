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

// setWorkspace stores the workspace path atomically. Writers hold r.mu; the
// atomic store lets lock-free readers (Workspace) stay race-free.
func (s *ManagedSession) setWorkspace(ws string) { storeAtomicString(&s.workspace, ws) }

// IsExempt returns whether this session is exempt from TTL and eviction.
func (s *ManagedSession) IsExempt() bool { return s.exempt }

// loadAtomicString and storeAtomicString are thin package-private wrappers
// around textutil.LoadAtomicString / StoreAtomicString; the behavioural
// contract (equal-value short-circuit, last-writer-wins) is documented there.
func loadAtomicString(v *atomic.Pointer[string]) string {
	return textutil.LoadAtomicString(v)
}

func storeAtomicString(v *atomic.Pointer[string], s string) {
	textutil.StoreAtomicString(v, s)
}

// loadTotalCost reads the float64 cumulative cost from an atomic.Uint64
// field (IEEE-754 bits). An unwritten field loads as 0 → float64 zero.
// Package-local until a second call site appears.
func loadTotalCost(v *atomic.Uint64) float64 {
	return math.Float64frombits(v.Load())
}

// storeTotalCost writes a float64 cumulative cost via atomic.Uint64; paired
// with loadTotalCost so the bit-packing convention lives in one place.
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
	// accessProfile is the resolved access-profile ID ("" = global default).
	// Packed here rather than in a separate atomic so resume-lock reads see
	// backend+profile — jointly the auth identity a dead session must resume
	// on — as one consistent snapshot (RFC project-access-profile §7).
	accessProfile string
}

// loadCLIIdentity returns a copy of the current identity box in one atomic
// Load. The zero box (all "") means nothing has been set; callers treat it
// as "use router default".
func (s *ManagedSession) loadCLIIdentity() cliIdentityBox {
	if box := s.cliIdentity.Load(); box != nil {
		return *box
	}
	return cliIdentityBox{}
}

// updateCLIIdentity is the CAS-loop primitive all Set* helpers funnel
// through: mut maps the current box (zero when unset) to the next one and
// the CAS retries until it wins, so concurrent writers (spawn / reconnect
// under r.mu, shim discovery) never drop each other's fields. Unchanged
// boxes short-circuit without a store.
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

// AccessProfile returns the access-profile ID this session spawned under
// ("" = global default). Used by resolveSpawnParamsLocked for resume-lock and
// by the store persister. RFC project-access-profile §7.
func (s *ManagedSession) AccessProfile() string { return s.loadCLIIdentity().accessProfile }

// SetAccessProfile records the resolved access-profile ID. Called at spawn
// time and by the store-restore path in NewRouter.
func (s *ManagedSession) SetAccessProfile(id string) {
	s.updateCLIIdentity(func(cur cliIdentityBox) cliIdentityBox {
		cur.accessProfile = id
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
// Daemon callers should prefer Router.SetUserLabelWithOrigin so LabelOrigin
// stays consistent; this bare setter is for router restore and tests.
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
// not yet captured from system/init / SpawnOptions).
func (s *ManagedSession) Model() string { return loadAtomicString(&s.model) }

// SetModel records the latest known model id (readLoop snapshotter and the
// store-restore path in NewRouter).
func (s *ManagedSession) SetModel(v string) { storeAtomicString(&s.model, v) }

// TuningModel returns the operator's per-session model override ("" = no
// override, config chain applies). docs/rfc/dashboard-model-effort-control.md §4.3.
func (s *ManagedSession) TuningModel() string { return loadAtomicString(&s.tuningModel) }

// SetTuningModel records the per-session model override. Callers must have
// validated via tuningspec.ValidateModel; "" clears the override. Writers:
// Router.SetSessionTuning (under r.mu) and the store-restore path.
func (s *ManagedSession) SetTuningModel(v string) { storeAtomicString(&s.tuningModel, v) }

// TuningEffort returns the operator's per-session thinking-effort override
// ("" = no override). docs/rfc/dashboard-model-effort-control.md §4.3.
func (s *ManagedSession) TuningEffort() string { return loadAtomicString(&s.tuningEffort) }

// SetTuningEffort records the per-session effort override. Callers must
// have validated via tuningspec.ValidateEffort; "" clears the override.
func (s *ManagedSession) SetTuningEffort(v string) { storeAtomicString(&s.tuningEffort, v) }

// SetHistorySource installs the backend-specific disk-tier Source. Atomic, so
// safe after publication, but a pagination request already in progress may
// not observe a mid-flight swap. nil disables disk fallback (history.Noop).
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
