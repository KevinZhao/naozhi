package sysession

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		isPanic   bool
		wantState DaemonRunState
		wantClass DaemonErrorClass
	}{
		{"nil error → success", nil, false, DaemonRunSucceeded, DaemonErrorClassNone},
		{"panic", fmt.Errorf("panicked: x"), true, DaemonRunFailed, DaemonErrorClassPanic},
		{"deadline → timeout", fmt.Errorf("wrap: %w", context.DeadlineExceeded), false, DaemonRunTimedOut, DaemonErrorClassTimeout},
		{"canceled → canceled", fmt.Errorf("wrap: %w", context.Canceled), false, DaemonRunCanceled, DaemonErrorClassNone},
		{"validation sentinel → validation", fmt.Errorf("title rejected: %w", ErrValidation), false, DaemonRunFailed, DaemonErrorClassValidation},
		{"random error → upstream", errors.New("kaboom"), false, DaemonRunFailed, DaemonErrorClassUpstream},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			gotState, gotClass := classifyError(c.err, c.isPanic)
			if gotState != c.wantState {
				t.Errorf("state = %q, want %q", gotState, c.wantState)
			}
			if gotClass != c.wantClass {
				t.Errorf("class = %q, want %q", gotClass, c.wantClass)
			}
		})
	}
}

func TestNewRunID_Format(t *testing.T) {
	t.Parallel()
	a, b := newRunID(), newRunID()
	if a == b {
		t.Errorf("newRunID returned identical values: %q", a)
	}
	if len(a) == 0 {
		t.Error("newRunID returned empty string")
	}
}
