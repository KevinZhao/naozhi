package cron

import (
	"context"
	"reflect"
	"testing"
)

// TestSchedulerConfigDepsBoundary pins the cfg/deps split ratified in RFC
// cron-sysession-merge §3.5.1 (#746): SchedulerConfig carries only value /
// scalar configuration, SchedulerDeps carries only injected components
// (interface / func / map types). If a new field lands on the wrong side,
// this test fails with a pointer to the rule.
//
// Exception H3 (§3.5.1): context.Context is a lifecycle scalar, not a
// component — ParentCtx is whitelisted on the cfg side even though its
// static type is an interface (same convention as http.Server.BaseContext).
func TestSchedulerConfigDepsBoundary(t *testing.T) {
	t.Parallel()

	// cfgInterfaceWhitelist lists SchedulerConfig fields allowed to have an
	// interface type despite the "no components in cfg" rule.
	cfgInterfaceWhitelist := map[string]bool{
		"ParentCtx": true, // H3: lifecycle scalar
	}
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()

	cfgType := reflect.TypeOf(SchedulerConfig{})
	for i := 0; i < cfgType.NumField(); i++ {
		f := cfgType.Field(i)
		switch f.Type.Kind() {
		case reflect.Interface:
			if !cfgInterfaceWhitelist[f.Name] {
				t.Errorf("SchedulerConfig.%s has interface type %v; components belong in SchedulerDeps (RFC §3.5.1, #746)", f.Name, f.Type)
			} else if f.Type != ctxType {
				t.Errorf("SchedulerConfig.%s is whitelisted as context.Context but has type %v", f.Name, f.Type)
			}
		case reflect.Func:
			t.Errorf("SchedulerConfig.%s has func type %v; components belong in SchedulerDeps (RFC §3.5.1, #746)", f.Name, f.Type)
		case reflect.Map:
			t.Errorf("SchedulerConfig.%s has map type %v; component maps belong in SchedulerDeps (RFC §3.5.1, #746)", f.Name, f.Type)
		}
	}

	depsType := reflect.TypeOf(SchedulerDeps{})
	if depsType.NumField() == 0 {
		t.Fatal("SchedulerDeps has no fields; expected the five injected components")
	}
	for i := 0; i < depsType.NumField(); i++ {
		f := depsType.Field(i)
		switch f.Type.Kind() {
		case reflect.Interface, reflect.Func, reflect.Map:
			// component-shaped: OK
		default:
			t.Errorf("SchedulerDeps.%s has value type %v; scalar/value configuration belongs in SchedulerConfig (RFC §3.5.1, #746)", f.Name, f.Type)
		}
	}
}
