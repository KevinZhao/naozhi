package platform

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestJitterBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Second},
		{"small", 10 * time.Millisecond},
		{"medium", 500 * time.Millisecond},
		{"large", 30 * time.Second},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.in <= 0 {
				if got := jitterBackoff(tc.in); got != tc.in {
					t.Fatalf("jitterBackoff(%v) = %v, want passthrough", tc.in, got)
				}
				return
			}
			// Factor is [0.75, 1.25); run enough samples to exercise both
			// ends of the range and assert every sample falls within bounds.
			lo := time.Duration(float64(tc.in) * 0.75)
			hi := time.Duration(float64(tc.in) * 1.25)
			for i := 0; i < 200; i++ {
				got := jitterBackoff(tc.in)
				if got < lo || got >= hi {
					t.Fatalf("sample %d: jitterBackoff(%v) = %v, want in [%v,%v)", i, tc.in, got, lo, hi)
				}
			}
		})
	}
}

// fakePermErr implements PermanentError with configurable flag; used to
// verify IsPermanent traverses wrapped chains.
type fakePermErr struct {
	msg  string
	perm bool
}

func (e *fakePermErr) Error() string     { return e.msg }
func (e *fakePermErr) IsPermanent() bool { return e.perm }

// permWrapper is a PermanentError that wraps another error. Used to pin
// the "first match wins" semantic of errors.As over the PermanentError
// interface.
type permWrapper struct {
	inner error
	perm  bool
}

func (e *permWrapper) Error() string     { return "wrap: " + e.inner.Error() }
func (e *permWrapper) IsPermanent() bool { return e.perm }
func (e *permWrapper) Unwrap() error     { return e.inner }

func TestIsPermanent(t *testing.T) {
	t.Parallel()
	permRoot := &fakePermErr{msg: "token revoked", perm: true}
	nonPermRoot := &fakePermErr{msg: "try again", perm: false}
	wrapped := errors.New("wrapper")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain_error", wrapped, false},
		{"permanent_direct", permRoot, true},
		{"non_permanent_direct", nonPermRoot, false},
		// errors.As walks chains; a permanent wrapped by a plain error
		// must still report permanent.
		{"permanent_wrapped_by_plain", fmt.Errorf("outer: %w", permRoot), true},
		// Known semantic: errors.As stops at the first match. A non-permanent
		// PermanentError wrapping a permanent one reports non-permanent.
		// No call site produces this shape today; pin the contract so any
		// future change is deliberate.
		{"first_match_wins", fmt.Errorf("outer: %w", &permWrapper{inner: permRoot, perm: false}), false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsPermanent(tc.err); got != tc.want {
				t.Fatalf("IsPermanent(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
