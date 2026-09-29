package cron

import (
	"reflect"
	"testing"
)

// schedulerMethodBaseline is *Scheduler's exported method count (#2955). A new
// method is surface every consumer can reach; fewer means the baseline is
// lowered in the same change, so the room cannot be refilled.
const schedulerMethodBaseline = 35

func TestSchedulerSurface_Ratchet(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[*Scheduler]()
	switch n := typ.NumMethod(); {
	case n > schedulerMethodBaseline:
		var names []string
		for i := range n {
			names = append(names, typ.Method(i).Name)
		}
		t.Errorf("*Scheduler has %d exported methods, above the baseline of %d: give a consumer interface only what it needs, or justify the method\n%v", n, schedulerMethodBaseline, names)
	case n < schedulerMethodBaseline:
		t.Errorf("*Scheduler has %d exported methods: lower schedulerMethodBaseline to %d", n, n)
	}
}
