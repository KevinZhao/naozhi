package session

// testObserver lets a test hook the router's notifications one at a time. In
// production the observer is fixed at construction; tests install this one
// before they drive the router.
type testObserver struct {
	changed func()
	retired func(key, sessionID string)
}

func (o *testObserver) SessionsChanged() {
	if o.changed != nil {
		o.changed()
	}
}

func (o *testObserver) KeyRetired(key, sessionID string) {
	if o.retired != nil {
		o.retired(key, sessionID)
	}
}

// observe returns r's testObserver, installing one on first use.
func observe(r *Router) *testObserver {
	if o, ok := r.observer.(*testObserver); ok {
		return o
	}
	o := &testObserver{}
	r.observer = o
	return o
}
