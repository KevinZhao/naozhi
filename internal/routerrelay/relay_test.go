package routerrelay

import (
	"strings"
	"testing"
)

// Unbound slots drop their events and deny ownership; bound ones forward.
func TestRelay_UnboundDropsBoundForwards(t *testing.T) {
	var r Relay
	r.SessionsChanged()
	r.KeyRetired("k", "s")
	if r.OwnsCostRun("cron:x") {
		t.Error("unbound OwnsCostRun answered true")
	}

	changes := 0
	var retired []string
	r.BindSessionsChanged(func() { changes++ })
	r.BindKeyRetired(func(key, sid string) { retired = append(retired, key+"/"+sid) })
	r.BindCostRunOwner(func(key string) bool { return key == "cron:x" })

	r.SessionsChanged()
	r.KeyRetired("k", "s")
	if changes != 1 || len(retired) != 1 || retired[0] != "k/s" {
		t.Errorf("after bind: changes %d, retired %v", changes, retired)
	}
	if !r.OwnsCostRun("cron:x") || r.OwnsCostRun("cron:y") {
		t.Error("OwnsCostRun does not consult the bound owner")
	}
}

// A second bind of any slot panics, naming the slot.
func TestRelay_BindTwicePanics(t *testing.T) {
	for name, bind := range map[string]func(*Relay){
		"SessionsChanged": func(r *Relay) { r.BindSessionsChanged(func() {}) },
		"KeyRetired":      func(r *Relay) { r.BindKeyRetired(func(string, string) {}) },
		"CostRunOwner":    func(r *Relay) { r.BindCostRunOwner(func(string) bool { return false }) },
	} {
		var r Relay
		bind(&r)
		func() {
			defer func() {
				p, _ := recover().(string)
				if !strings.Contains(p, name) {
					t.Errorf("%s: second bind panic = %q, want one naming the slot", name, p)
				}
			}()
			bind(&r)
		}()
	}
}
