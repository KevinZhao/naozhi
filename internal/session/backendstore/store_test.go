package backendstore

import (
	"reflect"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// A backend's configuration is ONE value: Store holds one map[backendID]→row
// rather than a column per property (G2 #2666). With parallel tables "this
// backend's configuration" could only be had by reading several in the right
// order, and #2668 was a second consumer reading a subset with different
// precedence.
func TestStoreHasOneKeyedTable(t *testing.T) {
	v := reflect.ValueOf(Store{})
	var maps []string
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.Map {
			maps = append(maps, v.Type().Field(i).Name)
		}
	}
	if !slices.Equal(maps, []string{"runtimes"}) {
		t.Errorf("Store map fields = %v, want exactly [runtimes]: add a property to Runtime instead", maps)
	}
}

// Reading an unconfigured backend gives the zero row, on a nil Store too:
// hand-built test routers reach this path.
func TestRuntime_UnknownIDYieldsZero(t *testing.T) {
	var nilStore *Store
	if got := nilStore.Runtime("x"); !reflect.DeepEqual(got, Runtime{}) {
		t.Errorf("nil Store Runtime = %+v, want zero", got)
	}
	s := New(Config{Runtimes: map[string]Runtime{"claude": {Wrapper: &cli.Wrapper{}}}})
	if got := s.Runtime("kiro"); !reflect.DeepEqual(got, Runtime{}) {
		t.Errorf("Runtime(unconfigured) = %+v, want zero", got)
	}
	if s.Runtime("claude").Wrapper == nil {
		t.Error("Runtime(configured).Wrapper is nil")
	}
}

// Runtime hands out a snapshot: mutating it does not reach the store.
func TestRuntime_ValueCopyDoesNotAliasTheStore(t *testing.T) {
	s := New(Config{Runtimes: map[string]Runtime{"claude": {Model: "opus"}}})
	snap := s.Runtime("claude")
	snap.Model = "mutated"
	if got := s.Runtime("claude").Model; got != "opus" {
		t.Errorf("store Model = %q after mutating a snapshot, want opus", got)
	}
	cfg := s.Config()
	cfg.Runtimes["claude"] = Runtime{Model: "edited"}
	if got := s.Runtime("claude").Model; got != "opus" {
		t.Errorf("store Model = %q after editing Config(), want opus", got)
	}
}

// The id → wrapper view feeds IDs and the shim manager list, which must not
// see a config-only backend as spawnable.
func TestWrappers_SkipsRowsWithoutOne(t *testing.T) {
	s := New(Config{Runtimes: map[string]Runtime{"claude": {Wrapper: &cli.Wrapper{}}, "kiro": {Model: "m"}}})
	got := s.Wrappers()
	if _, ok := got["kiro"]; ok {
		t.Error("Wrappers included a backend with no wrapper")
	}
	if _, ok := got["claude"]; !ok {
		t.Error("Wrappers dropped a backend that has one")
	}
}

// WrapperFor looks up the requested row, then the default backend's, then the
// fallback — and the id it returns is always that wrapper's own. Config-only
// rows resolve every request to the fallback under its own id.
func TestWrapperFor_PairsAWrapperWithItsOwnID(t *testing.T) {
	claude := &cli.Wrapper{BackendID: "claude"}
	kiro := &cli.Wrapper{BackendID: "kiro"}
	withRows := New(Config{Wrapper: claude, DefaultBackend: "claude",
		Runtimes: map[string]Runtime{"claude": {Wrapper: claude}, "kiro": {Wrapper: kiro}, "codex": {Model: "m"}}})
	configOnly := New(Config{Wrapper: claude, Runtimes: map[string]Runtime{"claude": {Model: "opus"}}})
	var empty *Store
	for _, c := range []struct {
		name   string
		s      *Store
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
		{"no store", empty, "kiro", nil, ""},
	} {
		w, id := c.s.WrapperFor(c.req)
		if w != c.wantW || id != c.wantID {
			t.Errorf("%s: WrapperFor(%q) = (%v, %q), want (%v, %q)", c.name, c.req, w, id, c.wantW, c.wantID)
		}
	}
}

// Wrapper answers from the rows: "" means the default backend, and a backend
// without a row (or with a config-only row) has none.
func TestWrapper_ReadsTheRows(t *testing.T) {
	claude := &cli.Wrapper{BackendID: "claude"}
	s := New(Config{Wrapper: claude, DefaultBackend: "claude",
		Runtimes: map[string]Runtime{"claude": {Wrapper: claude}, "codex": {Model: "m"}}})
	for _, c := range []struct {
		id   string
		want *cli.Wrapper
	}{{"", claude}, {"claude", claude}, {"codex", nil}, {"kiro", nil}} {
		if got := s.Wrapper(c.id); got != c.want {
			t.Errorf("Wrapper(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// New turns a lone Wrapper into a row under its own id ("claude" when empty)
// and makes it the default.
func TestNew_LoneWrapperBecomesARow(t *testing.T) {
	for _, c := range []struct{ backendID, wantID string }{{"", "claude"}, {"kiro", "kiro"}} {
		w := &cli.Wrapper{BackendID: c.backendID}
		s := New(Config{Wrapper: w})
		if s.Runtime(c.wantID).Wrapper != w || s.DefaultID() != c.wantID || !slices.Equal(s.IDs(), []string{c.wantID}) {
			t.Errorf("lone wrapper %q: row=%v default=%q ids=%v", c.backendID, s.Runtime(c.wantID).Wrapper, s.DefaultID(), s.IDs())
		}
	}
}

// With no DefaultBackend, New picks the first backend (sorted) that has a
// wrapper — deterministically, whatever the map order.
func TestNew_PicksTheDefaultDeterministically(t *testing.T) {
	a, b := &cli.Wrapper{BackendID: "alpha"}, &cli.Wrapper{BackendID: "beta"}
	for i := 0; i < 20; i++ {
		s := New(Config{Runtimes: map[string]Runtime{"beta": {Wrapper: b}, "alpha": {Wrapper: a}, "aaa": {Model: "config only"}}})
		if s.DefaultID() != "alpha" || s.Fallback() != a {
			t.Fatalf("default = %q fallback = %v, want alpha", s.DefaultID(), s.Fallback())
		}
		if !slices.Equal(s.IDs(), []string{"alpha", "beta"}) {
			t.Fatalf("IDs = %v, want [alpha beta]", s.IDs())
		}
	}
}

// IDs puts the default first and hands out a copy.
func TestIDs_DefaultFirstAndCopied(t *testing.T) {
	s := New(Config{DefaultBackend: "kiro", Runtimes: map[string]Runtime{
		"claude": {Wrapper: &cli.Wrapper{}}, "kiro": {Wrapper: &cli.Wrapper{}}, "codex": {Wrapper: &cli.Wrapper{}}}})
	ids := s.IDs()
	if !slices.Equal(ids, []string{"kiro", "claude", "codex"}) {
		t.Fatalf("IDs = %v, want [kiro claude codex]", ids)
	}
	ids[0] = "mutated"
	if s.IDs()[0] != "kiro" {
		t.Error("IDs returned the cached slice, not a copy")
	}
}

// Default falls back to the fallback wrapper's own backend when New had no
// default to record.
func TestDefault_FallsBackToTheWrapper(t *testing.T) {
	w := &cli.Wrapper{BackendID: "claude"}
	s := New(Config{Wrapper: w, Runtimes: map[string]Runtime{"kiro": {Model: "m"}}})
	if s.DefaultID() != "" || s.Default() != "claude" {
		t.Errorf("DefaultID = %q, Default = %q; want \"\" and claude", s.DefaultID(), s.Default())
	}
}

// New(s.Config()) rebuilds an identical table.
func TestConfig_RoundTrips(t *testing.T) {
	w := &cli.Wrapper{BackendID: "claude"}
	s := New(Config{Wrapper: w, Model: "m", ExtraArgs: []string{"--a"},
		Runtimes: map[string]Runtime{"claude": {Wrapper: w, Effort: "high"}, "kiro": {Model: "k"}}})
	again := New(s.Config())
	if !reflect.DeepEqual(again.Config(), s.Config()) || !slices.Equal(again.IDs(), s.IDs()) {
		t.Errorf("round trip differs:\n%+v\n%+v", again.Config(), s.Config())
	}
}

func TestManifest_CachedPerBackend(t *testing.T) {
	s := New(Config{})
	s.SetManifest("kiro", []cli.ModelInfo{{ID: "a"}})
	if got := s.Manifest("kiro"); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("Manifest(kiro) = %v", got)
	}
	if s.Manifest("claude") != nil {
		t.Error("another backend saw kiro's manifest")
	}
	var nilStore *Store
	nilStore.SetManifest("x", nil)
	if nilStore.Manifest("x") != nil || nilStore.IDs() != nil || nilStore.Wrappers() != nil || nilStore.Fallback() != nil || nilStore.Default() != "" {
		t.Error("a nil Store must answer like an empty one")
	}
}
