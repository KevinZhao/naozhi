package session

import "testing"

// The observer comes from RouterConfig, and the router reaches it for every
// notification.
func TestNewRouter_ObserverFromConfig(t *testing.T) {
	o := &testObserver{}
	changes, retired := 0, ""
	o.changed = func() { changes++ }
	o.retired = func(key, sid string) { retired = key + "/" + sid }
	r := NewRouter(RouterConfig{Observer: o})
	t.Cleanup(r.Shutdown)

	r.notifyChange()
	r.notifyKeyRetired("k", "s")
	if changes != 1 || retired != "k/s" {
		t.Errorf("observer saw changes=%d retired=%q", changes, retired)
	}
}
