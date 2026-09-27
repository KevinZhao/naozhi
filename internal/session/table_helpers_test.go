package session

// Test shorthands for one read or write of the router's table, each run as
// its own transaction. They must not be called from inside a View or
// Update (the table lock is not reentrant).

func lookupT(r *Router, key string) (s *ManagedSession, ok bool) {
	r.ss.View(func(v sessView) { s, ok = v.Lookup(key) })
	return s, ok
}

func lenT(r *Router) (n int) {
	r.ss.View(func(v sessView) { n = v.Len() })
	return n
}

func keyForIDT(r *Router, id string) (key string, ok bool) {
	r.ss.View(func(v sessView) { key, ok = v.KeyForID(id) })
	return key, ok
}

func putT(r *Router, key string, s *ManagedSession) {
	r.ss.Update(func(tx sessTx) { tx.Put(key, s) })
}

func setIDT(r *Router, id, key string) {
	r.ss.Update(func(tx sessTx) { tx.SetID(id, key) })
}

func setActiveT(r *Router, n int64) {
	r.ss.Update(func(tx sessTx) { tx.SetActive(n) })
}

func publishT(r *Router, key string, s *ManagedSession, alreadyAttached bool) {
	r.ss.Update(func(tx sessTx) { r.publishSession(tx, key, s, alreadyAttached) })
}

func unregisterT(r *Router, key string, s *ManagedSession, keepBackendOverride bool) {
	r.ss.Update(func(tx sessTx) { r.unregisterSession(tx, key, s, keepBackendOverride) })
}

func resolveT(r *Router, key, resumeID string, opts AgentOpts) (sp spawnParams) {
	r.ss.Update(func(tx sessTx) { sp = r.resolveSpawnParams(tx, key, resumeID, opts) })
	return sp
}

// stateOf returns the router's table state for a test to set up or inspect
// directly. The caller reads and writes it without the table lock, so it is
// only for tests that are single-threaded at that point (-race reports a
// test that is not).
func stateOf(r *Router) (x *routerState) {
	r.ss.View(func(v sessView) { x = v.Ext() })
	return x
}

// keyForID is the key an ID is indexed under, "" when it is not indexed.
func keyForID(r *Router, id string) string {
	k, _ := keyForIDT(r, id)
	return k
}

// keyIn is keyForID for code already inside a transaction.
func keyIn(v sessView, id string) string {
	k, _ := v.KeyForID(id)
	return k
}
