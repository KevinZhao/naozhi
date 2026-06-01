package cron

import (
	"reflect"
	"testing"
)

// TestWithJobOpTypes_R249_ARCH_20 pins the #985 fix: the in-lock mutation and
// the out-of-lock side-effect hooks are now distinct NAMED types (lockedJobOp
// vs jobSideEffect) rather than two bare func(*Job)/func(*Job) error literals,
// so a swapped op-vs-cleanup argument is a compile error and the roles are
// self-documenting. This test guards the field types on both opts structs.
func TestWithJobOpTypes_R249_ARCH_20(t *testing.T) {
	t.Parallel()

	// lockedJobOp returns an error and runs under s.mu; jobSideEffect returns
	// nothing and runs lock-free. Assignability from plain closures must hold
	// (this is how every call site passes its op / cleanup).
	var op lockedJobOp = func(_ *Job) error { return nil }
	var fx jobSideEffect = func(_ *Job) {}
	_, _ = op, fx

	// The two opts structs must carry the named types, not bare func literals,
	// so future fields stay role-typed.
	idOpts := reflect.TypeOf(withJobByIDOpts{})
	for _, f := range []struct {
		field string
		want  string
	}{
		{"op", "lockedJobOp"},
		{"postCleanup", "jobSideEffect"},
		{"rollbackOnPersistErr", "jobSideEffect"},
	} {
		sf, ok := idOpts.FieldByName(f.field)
		if !ok {
			t.Fatalf("withJobByIDOpts missing field %q", f.field)
		}
		if got := sf.Type.Name(); got != f.want {
			t.Errorf("withJobByIDOpts.%s type = %q, want named type %q", f.field, got, f.want)
		}
	}

	prefixOpts := reflect.TypeOf(withJobByPrefixOpts{})
	sf, ok := prefixOpts.FieldByName("rollbackOnPersistErr")
	if !ok {
		t.Fatal("withJobByPrefixOpts missing rollbackOnPersistErr")
	}
	if got := sf.Type.Name(); got != "jobSideEffect" {
		t.Errorf("withJobByPrefixOpts.rollbackOnPersistErr type = %q, want jobSideEffect", got)
	}
}
