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

// wrapperFor has one path: the requested backend's row, then the default
// backend's, then the router's fallback wrapper — and whatever it returns, the
// id is that wrapper's own. A router whose rows come from config alone (no row
// holds a wrapper) resolves every request to the fallback under its own id,
// never under the requested one.
func TestWrapperFor_PairsAWrapperWithItsOwnID(t *testing.T) {
	claude := &cli.Wrapper{BackendID: "claude"}
	kiro := &cli.Wrapper{BackendID: "kiro"}

	withRows := &Router{ss: newSessionTable()}
	withRows.bkStore.wrapper = claude
	withRows.bkStore.defaultBackend = "claude"
	withRows.bkStore.initRuntimes(map[string]BackendRuntime{"claude": {Wrapper: claude}, "kiro": {Wrapper: kiro}, "codex": {Model: "m"}})

	configOnly := &Router{ss: newSessionTable()}
	configOnly.bkStore.wrapper = claude
	configOnly.bkStore.initRuntimes(map[string]BackendRuntime{"claude": {Model: "opus"}})

	empty := &Router{ss: newSessionTable()}

	for _, c := range []struct {
		name   string
		r      *Router
		req    string
		wantW  *cli.Wrapper
		wantID string
	}{
		{"known backend", withRows, "kiro", kiro, "kiro"},
		{"empty is the default", withRows, "", claude, "claude"},
		{"unknown falls back to the default", withRows, "gemini", claude, "claude"},
		{"config-only row falls back to the default", withRows, "codex", claude, "claude"},
		{"config only, empty", configOnly, "", claude, "claude"},
		{"config only, another backend", configOnly, "kiro", claude, "claude"},
		{"no wrapper at all", empty, "kiro", nil, ""},
	} {
		w, id := c.r.wrapperFor(c.req)
		if w != c.wantW || id != c.wantID {
			t.Errorf("%s: wrapperFor(%q) = (%v, %q), want (%v, %q)", c.name, c.req, w, id, c.wantW, c.wantID)
		}
	}
}

// BackendWrapper answers from the rows: "" means the default backend, and a
// backend without a row (or with a config-only row) has no wrapper.
func TestBackendWrapper_ReadsTheRows(t *testing.T) {
	claude := &cli.Wrapper{BackendID: "claude"}
	r := &Router{ss: newSessionTable()}
	r.bkStore.wrapper = claude
	r.bkStore.defaultBackend = "claude"
	r.bkStore.initRuntimes(map[string]BackendRuntime{"claude": {Wrapper: claude}, "codex": {Model: "m"}})
	for _, c := range []struct {
		id   string
		want *cli.Wrapper
	}{{"", claude}, {"claude", claude}, {"codex", nil}, {"kiro", nil}} {
		if got := r.BackendWrapper(c.id); got != c.want {
			t.Errorf("BackendWrapper(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}
