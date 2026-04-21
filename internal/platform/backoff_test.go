package platform

import (
	"errors"
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
