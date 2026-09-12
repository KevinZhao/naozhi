package session

import (
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestBackendStoreHasOneKeyedTable is the structural point of G2 #2666: a
// backend's configuration is ONE value, so backendStore holds one
// map[backendID]→row rather than a column per property.
//
// The failure mode being prevented is not untidiness. With six parallel tables,
// "this backend's configuration" could only be had by reading several in the
// right order, and #2668 was a second consumer reading a subset with different
// precedence — reporting healthy sessions as drifted and telling the operator to
// restart them.
func TestBackendStoreHasOneKeyedTable(t *testing.T) {
	var bs backendStore
	v := reflect.ValueOf(&bs).Elem()
	var maps []string
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.Map {
			maps = append(maps, v.Type().Field(i).Name)
		}
	}
	if len(maps) != 1 || maps[0] != "runtimes" {
		t.Errorf("backendStore map fields = %v, want exactly [runtimes].\n"+
			"A new map[backendID]→property is the column storage this replaced: add the "+
			"property to BackendRuntime instead, so one backend stays one value.", maps)
	}
}

// TestBackendRuntime_UnknownIDYieldsZero pins the behaviour the six maps had for
// a missing key, which every reader depended on: reading an unconfigured
// backend's property gave the zero value, not a panic.
//
// runtime() returns a VALUE for this reason. A *BackendRuntime would put a nil
// deref between the caller and that behaviour, which is the shape behind #377,
// #2551 and #2561.
func TestBackendRuntime_UnknownIDYieldsZero(t *testing.T) {
	var bs backendStore
	// Deliberately before any init: a zero backendStore must answer too, because
	// hand-built test routers reach this path.
	got := bs.runtime("never-configured")
	if !reflect.DeepEqual(got, BackendRuntime{}) {
		t.Errorf("runtime(unknown) = %+v, want the zero value", got)
	}

	bs.initRuntimes(map[string]BackendRuntime{"claude": {Wrapper: &cli.Wrapper{}}})
	if got := bs.runtime("kiro"); !reflect.DeepEqual(got, BackendRuntime{}) {
		t.Errorf("runtime(unconfigured) = %+v, want the zero value", got)
	}
	if bs.runtime("claude").Wrapper == nil {
		t.Error("runtime(configured).Wrapper is nil; initRuntimes did not store the wrapper")
	}
}

// TestBackendRuntime_ValueCopyDoesNotAliasTheStore: runtime() hands out a
// snapshot, so a caller mutating what it got must not reach into the store. The
// one path that legitimately mutates goes through runtimeMut under the write lock.
func TestBackendRuntime_ValueCopyDoesNotAliasTheStore(t *testing.T) {
	var bs backendStore
	bs.initRuntimes(map[string]BackendRuntime{"claude": {Model: "opus"}})

	snap := bs.runtime("claude")
	snap.Model = "mutated"
	if got := bs.runtime("claude").Model; got != "opus" {
		t.Errorf("store Model = %q after mutating a snapshot, want %q", got, "opus")
	}

	bs.runtimeMut("claude").Model = "written"
	if got := bs.runtime("claude").Model; got != "written" {
		t.Errorf("runtimeMut did not write through: Model = %q", got)
	}
}

// TestBackendRuntime_PerBackendWrappersIsNotLenRuntimes guards the one trap in
// this migration. wrapperFor's legacy single-wrapper branch used to key on
// len(wrappers) == 0; runtimes can be non-empty from CONFIG alone, so deriving
// the branch from len(runtimes) would silently stop taking it.
func TestBackendRuntime_PerBackendWrappersIsNotLenRuntimes(t *testing.T) {
	var bs backendStore
	// Config for a backend, no wrappers at all — the legacy shape.
	bs.initRuntimes(map[string]BackendRuntime{"claude": {Model: "opus"}})
	if len(bs.runtimes) == 0 {
		t.Fatal("fixture is wrong: config alone should have created a row")
	}
	if bs.perBackendWrappers {
		t.Error("perBackendWrappers is true with no wrappers supplied; wrapperFor would skip " +
			"its legacy single-wrapper branch and return a nil wrapper for the default backend")
	}

	bs.initRuntimes(map[string]BackendRuntime{"claude": {Wrapper: &cli.Wrapper{}}})
	if !bs.perBackendWrappers {
		t.Error("perBackendWrappers is false after wrappers were supplied")
	}
}

// TestBackendRuntime_BackendWrappersSkipsRowsWithoutOne: the id→wrapper
// projection feeds computeBackendIDs and shimManagers, which must not see a
// config-only backend as spawnable.
func TestBackendRuntime_BackendWrappersSkipsRowsWithoutOne(t *testing.T) {
	var bs backendStore
	bs.initRuntimes(map[string]BackendRuntime{
		"claude": {Wrapper: &cli.Wrapper{}},
		"kiro":   {Model: "kiro-model"}, // config only, no wrapper
	})
	got := bs.backendWrappers()
	if _, ok := got["kiro"]; ok {
		t.Error("backendWrappers included a backend with no wrapper; it is not spawnable")
	}
	if _, ok := got["claude"]; !ok {
		t.Error("backendWrappers dropped a backend that has one")
	}
}

// TestWrapperFor_LegacyBranchSurvivesConfigOnlyRows is the BEHAVIOURAL half of
// the trap above, and getting it to actually bite took two attempts.
//
// wrapperFor's legacy single-wrapper branch used to key on len(wrappers) == 0.
// runtimes can be non-empty from CONFIG alone, so deriving the branch from
// len(runtimes) skips it — but the function's last-resort clause then returns the
// same wrapper anyway, so a fixture that only checks the wrapper passes either
// way. The first version of this test did exactly that and the probe sailed
// through.
//
// What differs is the effective BACKEND ID. The legacy branch answers with the
// REQUESTED id (one wrapper serves every backend); the last-resort clause answers
// with the wrapper's OWN id. That id is stamped on the session and is what the
// drift view and history wiring key on, so returning "claude" where the caller
// asked for "kiro" is a real divergence — just a quiet one.
func TestWrapperFor_LegacyBranchSurvivesConfigOnlyRows(t *testing.T) {
	legacy := &cli.Wrapper{BackendID: "claude"}
	r := &Router{}
	r.bkStore.wrapper = legacy
	// Config for a backend, no wrappers map — exactly the legacy shape.
	r.bkStore.initRuntimes(map[string]BackendRuntime{"claude": {Model: "opus"}})

	cases := []struct{ req, wantID string }{
		{"", "claude"}, // empty → the wrapper's own id
		{"claude", "claude"},
		{"kiro", "kiro"}, // the discriminating case: requested id is preserved
	}
	for _, c := range cases {
		w, id := r.wrapperFor(c.req)
		if w != legacy {
			t.Errorf("wrapperFor(%q) wrapper = %v, want the legacy default", c.req, w)
		}
		if id != c.wantID {
			t.Errorf("wrapperFor(%q) id = %q, want %q. runtimes is non-empty from config "+
				"alone, so the legacy branch must key on perBackendWrappers — under "+
				"len(runtimes) the last-resort clause answers with the wrapper's own id "+
				"instead of the requested one.", c.req, id, c.wantID)
		}
	}
}
