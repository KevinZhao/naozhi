package session

import "testing"

// The observer and the cost-run owner come from RouterConfig, and the router
// reaches them for every notification and ownership question.
func TestNewRouter_ObserverAndCostRunOwnerFromConfig(t *testing.T) {
	o := &testObserver{}
	changes, retired := 0, ""
	o.changed = func() { changes++ }
	o.retired = func(key, sid string) { retired = key + "/" + sid }
	r := NewRouter(RouterConfig{
		Observer:     o,
		CostRunOwner: func(key string) bool { return key == "cron:j" },
	})
	t.Cleanup(r.Shutdown)

	r.notifyChange()
	r.notifyKeyRetired("k", "s")
	if changes != 1 || retired != "k/s" {
		t.Errorf("observer saw changes=%d retired=%q", changes, retired)
	}
	if !r.costAcct.owned("cron:j") || r.costAcct.owned("cron:other") {
		t.Error("cost-run ownership does not consult RouterConfig.CostRunOwner")
	}
}
