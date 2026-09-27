package session

// pending_picks.go — the dashboard's per-session choices for a key that may not
// have a ManagedSession yet (G2 #2666).
//
// These three maps used to live in backendStore, which is keyed by BACKEND ID.
// They are keyed by full SESSION KEY, so every place that manipulates a session
// key had to reach into the backend facet and remember all three: RenameSession
// carried a twelve-line block moving each map's entry by hand, and terminal
// removal deleted each one separately. Forgetting one leaks a stale pick onto the
// next spawn — and one path already forgets two (see dropBackend).
//
// Moving them here removes three of backendStore's nine columns and, more to the
// point, gives the maintenance a name: rename and drop are one call each instead
// of three open-coded map operations that must agree.

// pendingPicks holds per-session-key choices made before (or independently of)
// the session's ManagedSession existing. It lives in the router's table state,
// so every method runs inside a table transaction.
//
// Each pick is consumed by the key's next spawn, and the session then carries
// the choice itself (a respawn of an existing session reuses its backend,
// access profile and tuning). They stay three maps because they are consumed
// at different points:
//
//   - backend and accessProfile are consumed when the spawn is reserved
//     (read-and-delete in resolveSpawnParams), so a spawn that then fails has
//     used them up.
//   - tuning is consumed only once the spawn succeeds (consumePendingTuning in
//     the commit), so a failed spawn leaves it for the retry.
//
// accessProfile gates which credentials a spawn gets (RFC
// project-access-profile §8.2), so when it clears matters.
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

// init allocates the maps. Separate from a constructor because Router is
// assembled field-by-field in NewRouter.
func (p *pendingPicks) init() {
	p.backend = make(map[string]string)
	p.accessProfile = make(map[string]string)
	p.tuning = make(map[string]pendingTuning)
}

// rename moves every pick from oldKey to newKey. One call instead of the
// three open-coded move blocks RenameSession used to carry — the failure mode
// being a fourth pick added later and renamed in only two of the three places.
func (p *pendingPicks) rename(oldKey, newKey string) {
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

// dropAll clears every pick for key. Used on terminal removal, where an
// abandoned choice must not survive to be consumed by an unrelated future
// session that happens to reuse the key.
func (p *pendingPicks) dropAll(key string) {
	delete(p.backend, key)
	delete(p.accessProfile, key)
	delete(p.tuning, key)
}

// dropBackend clears only the backend pick, which is what the chat reset
// (ResetChatAndSetWorkspace, IM /cd) does; the per-key resets (/new, /clear)
// drop every pick.
//
// It deliberately leaves accessProfile and tuning: both are consumed on the
// first spawn, so normally there is nothing to clear, and in the window where
// there IS (a pick made before the first message, then /cd) the pick was made
// for THIS key and still applies to the next spawn. Only the backend pick
// resets, so the chat returns to the default backend.
func (p *pendingPicks) dropBackend(key string) {
	delete(p.backend, key)
}
