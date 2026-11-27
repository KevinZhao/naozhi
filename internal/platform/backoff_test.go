package platform

import (
	"errors"
	"fmt"
	"testing"
)

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
		// errors.As walks chains; a permanent wrapped by a plain error
		// must still report permanent.
		{"permanent_wrapped_by_plain", fmt.Errorf("outer: %w", permRoot), true},
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
