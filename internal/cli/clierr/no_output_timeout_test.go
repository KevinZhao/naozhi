package clierr

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// Every classifier matches the watchdog kill with errors.Is(ErrNoOutputTimeout);
// the typed error must keep that true, also when wrapped further up.
func TestNoOutputTimeoutError_IsSentinel(t *testing.T) {
	for _, err := range []error{
		&NoOutputTimeoutError{Timeout: time.Minute},
		&NoOutputTimeoutError{Timeout: time.Minute, Tool: "Bash", ToolElapsed: 2 * time.Minute},
		fmt.Errorf("send: %w", &NoOutputTimeoutError{Timeout: time.Minute, Tool: "Bash"}),
	} {
		if !errors.Is(err, ErrNoOutputTimeout) {
			t.Errorf("errors.Is(%v, ErrNoOutputTimeout) = false", err)
		}
		if errors.Is(err, ErrTotalTimeout) {
			t.Errorf("errors.Is(%v, ErrTotalTimeout) = true", err)
		}
		var nt *NoOutputTimeoutError
		if !errors.As(err, &nt) {
			t.Errorf("errors.As(%v) found no *NoOutputTimeoutError", err)
		}
	}
}

func TestNoOutputTimeoutError_Error(t *testing.T) {
	cases := []struct {
		err  *NoOutputTimeoutError
		want string
	}{
		{&NoOutputTimeoutError{Timeout: 15 * time.Minute}, "no output timeout (15m0s)"},
		{&NoOutputTimeoutError{Timeout: 15 * time.Minute, Tool: "Bash", ToolElapsed: 16*time.Minute + 400*time.Millisecond},
			`no output timeout (15m0s, tool "Bash" running 16m0s)`},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}
