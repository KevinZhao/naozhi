package session

// pickedBackendForTest returns the one-shot backend pick recorded for key by
// SetSessionBackend, or "" if none. Production code consumes the pick inside
// the spawn transaction and never reads it back, so the reader lives here
// rather than on Router.
func pickedBackendForTest(r *Router, key string) (backend string) {
	r.ss.View(func(v sessView) { backend = v.Ext().PickedBackend(key) })
	return backend
}

// pickedAccessProfileForTest is pickedBackendForTest for the access-profile
// pick recorded by SetSessionAccessProfile.
func pickedAccessProfileForTest(r *Router, key string) (profile string) {
	r.ss.View(func(v sessView) { profile = v.Ext().PickedAccessProfile(key) })
	return profile
}
