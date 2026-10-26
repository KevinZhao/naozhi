package project

import "testing"

// TestNewDataSource_NilManagerReturnsNilInterface asserts the defense
// against typed-nil interface values documented in
// docs/rfc/key-resolver.md §3.4. Returning `&dataSource{m: nil}` would
// make `data != nil` pass (typed-nil interface) but crash on first
// method call; `return nil` returns untyped nil instead.
func TestNewDataSource_NilManagerReturnsNilInterface(t *testing.T) {
	t.Parallel()
	if got := NewDataSource(nil); got != nil {
		t.Errorf("NewDataSource(nil) = %#v, want untyped nil", got)
	}
}

// TestNewDataSource_NonNilManager returns a usable adapter.
func TestNewDataSource_NonNilManager(t *testing.T) {
	t.Parallel()
	// NewManager without Scan returns an empty Manager; methods all
	// return nil / zero-value, which is fine for this smoke.
	m := &Manager{projects: map[string]*Project{}}
	ds := NewDataSource(m)
	if ds == nil {
		t.Fatal("NewDataSource with non-nil Manager returned nil")
	}

	// Unbound chat → zero ProjectBinding
	b := ds.ProjectBinding("feishu", "direct", "alice")
	if b.Bound {
		t.Errorf("expected Bound=false for empty manager, got %+v", b)
	}

	// Missing project → ok=false
	if _, ok := ds.ProjectByName("missing"); ok {
		t.Error("expected ok=false for missing project")
	}
}
