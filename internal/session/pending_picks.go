package session

// pending_picks.go — the dashboard's per-session choices for a key that may not
// have a ManagedSession yet (G2 #2666).
//
// These three maps used to live in backendStore, which is keyed by BACKEND ID.
// They are keyed by full SESSION KEY, so every place that manipulates a session
// key had to reach into the backend facet and remember all three: RenameSession
// carried a twelve-line block moving each map's entry by hand, and terminal
// removal deleted each one separately. Forgetting one leaks a stale pick onto the
// next spawn — and one path already forgets two (see dropBackendLocked).
//
// Moving them here removes three of backendStore's nine columns and, more to the
// point, gives the maintenance a name: rename and drop are one call each instead
// of three open-coded map operations that must agree.

// pendingPicks holds per-session-key choices made before (or independently of)
// the session's ManagedSession existing. Caller holds r.mu for every method:
// these are Router state and the Locked suffix follows the package convention.
//
// The three have DIFFERENT lifecycles, which is why they stay three maps rather
// than one struct:
//
//   - backend PERSISTS. resolveSpawnParams reads it on every spawn, so a session
//     that resets keeps the chosen backend. Dropped at reset and terminal removal.
//   - accessProfile is CONSUMED on the first spawn (read-and-delete in
//     resolveSpawnParams), then gone.
//   - tuning is CONSUMED on the first spawn (consumePendingTuningLocked), then
//     gone.
//
// The comments this replaced said accessProfile was "one-shot like
// backendOverrides" and had "the same lifecycle as backendOverrides". Both were
// wrong in the same direction: backend is the one that is NOT one-shot.
// accessProfile's real twin is tuning. That matters because accessProfile gates
// which credentials a spawn gets (RFC project-access-profile §8.2), so a reader
// who trusted the comment would have the wrong model of when it clears.
type pendingPicks struct {
	// backend: per-session backend picks keyed by full session key (with agent
	// suffix) so two sessions on one chat can run different backends.
	backend map[string]string
	// accessProfile: per-session access-profile picks (RFC
	// project-access-profile §8.2). Empty value = global default.
	accessProfile map[string]string
	// tuning: model/effort picked for a session that has no ManagedSession yet
	// (dashboard header chip before the first message). Values are
	// tuningspec-validated at write. docs/rfc/dashboard-model-effort-control.md §4.3.
	tuning map[string]pendingTuning
}

// initLocked allocates the maps. Separate from a constructor because Router is
// assembled field-by-field in NewRouter.
func (p *pendingPicks) initLocked() {
	p.backend = make(map[string]string)
	p.accessProfile = make(map[string]string)
	p.tuning = make(map[string]pendingTuning)
}

// renameLocked moves every pick from oldKey to newKey. One call instead of the
// three open-coded move blocks RenameSession used to carry — the failure mode
// being a fourth pick added later and renamed in only two of the three places.
func (p *pendingPicks) renameLocked(oldKey, newKey string) {
	if b, ok := p.backend[oldKey]; ok {
		p.backend[newKey] = b
		delete(p.backend, oldKey)
	}
	if ap, ok := p.accessProfile[oldKey]; ok {
		p.accessProfile[newKey] = ap
		delete(p.accessProfile, oldKey)
	}
	if pt, ok := p.tuning[oldKey]; ok {
		p.tuning[newKey] = pt
		delete(p.tuning, oldKey)
	}
}

// dropAllLocked clears every pick for key. Used on terminal removal, where an
// abandoned choice must not survive to be consumed by an unrelated future
// session that happens to reuse the key.
func (p *pendingPicks) dropAllLocked(key string) {
	delete(p.backend, key)
	delete(p.accessProfile, key)
	delete(p.tuning, key)
}

// dropBackendLocked clears only the backend pick, which is what the ResetChat
// path (/new, /clear) does.
//
// It deliberately leaves accessProfile and tuning: both are consumed on the
// first spawn, so normally there is nothing to clear, and in the window where
// there IS (a dashboard pick made before the first message, then /new) the pick
// was made for THIS key and still applies to the next spawn. Only the backend
// pick resets, so /new returns to the default backend.
//
// This asymmetry was previously an open-coded single delete with a comment
// mentioning only backendOverrides; naming it records that the other two are
// omitted on purpose rather than forgotten.
func (p *pendingPicks) dropBackendLocked(key string) {
	delete(p.backend, key)
}
