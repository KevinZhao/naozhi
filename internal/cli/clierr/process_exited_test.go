package clierr

import (
	"errors"
	"fmt"
	"testing"
)

// Every caller matches a dead CLI with errors.Is(ErrProcessExited); the typed
// error must keep that true, also when wrapped further up.
func TestProcessExitedError_IsSentinel(t *testing.T) {
	t.Parallel()
	typed := &ProcessExitedError{Code: 1, Class: ExitAuth}
	for _, err := range []error{typed, fmt.Errorf("process dead: %w", typed)} {
		if !errors.Is(err, ErrProcessExited) {
			t.Errorf("errors.Is(%v, ErrProcessExited) = false", err)
		}
		var pe *ProcessExitedError
		if !errors.As(err, &pe) || pe.Class != ExitAuth {
			t.Errorf("errors.As(%v) = %+v, want the typed error", err, pe)
		}
	}
	if got, want := typed.Error(), "process exited during send (code 1)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
