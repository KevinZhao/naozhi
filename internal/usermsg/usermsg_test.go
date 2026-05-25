package usermsg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// TestUserMessage_TimeoutSpecialisation locks in the contract that
// UserMessage renders the configured per-session no-output / total
// timeouts in Chinese and falls through to ForSendError for everything
// else. R249-DISPATCH-1 (#419) extracted this helper so the IM dispatch
// path stops keeping a parallel switch with cross-package "keep in sync"
// comments — regression here means new error kinds will start drifting
// between dispatch.go and server/errors_usermsg.go again.
func TestUserMessage_TimeoutSpecialisation(t *testing.T) {
	const noOutput = 90 * time.Second
	const total = 5 * time.Minute

	tests := []struct {
		name        string
		err         error
		key         string
		wantSubstrs []string
		notSubstrs  []string
	}{
		{
			name:        "no-output timeout renders configured duration",
			err:         cli.ErrNoOutputTimeout,
			wantSubstrs: []string{"无输出", "1 分钟 30 秒"},
			notSubstrs:  []string{"⏱️"}, // emoji is caller-decorated, not in helper.
		},
		{
			name:        "total timeout renders configured duration",
			err:         cli.ErrTotalTimeout,
			wantSubstrs: []string{"总耗时超过", "5 分钟"},
			notSubstrs:  []string{"⏱️"},
		},
		{
			name:        "context cancelled falls through to ForSendError",
			err:         context.Canceled,
			wantSubstrs: []string{"系统正在重启"},
		},
		{
			name:        "unknown error falls through to /new hint",
			err:         errors.New("boom"),
			wantSubstrs: []string{"/new"},
		},
		{
			name:        "cron-key NoActiveProcess routes through ForSendError key path",
			err:         session.ErrNoActiveProcess,
			key:         "cron:slug",
			wantSubstrs: []string{"定时任务"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UserMessage(tt.err, tt.key, noOutput, total)
			for _, want := range tt.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("UserMessage(%v) = %q, missing %q", tt.err, got, want)
				}
			}
			for _, bad := range tt.notSubstrs {
				if strings.Contains(got, bad) {
					t.Errorf("UserMessage(%v) = %q, must not contain %q (callers decorate)", tt.err, got, bad)
				}
			}
		})
	}
}

// TestUserMessage_ZeroTimeoutDoesNotPanic guards against a future
// caller that wires zero durations (e.g. a unit test using the helper
// without setting watchdog config). textutil.FormatChineseDuration
// returns "未知" for zero/negative values — this test pins that the
// helper still returns a non-empty error label so users get *some*
// hint instead of an empty IM bubble.
func TestUserMessage_ZeroTimeoutDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("UserMessage panicked on zero timeouts: %v", r)
		}
	}()
	got := UserMessage(cli.ErrNoOutputTimeout, "", 0, 0)
	if got == "" {
		t.Errorf("UserMessage with zero timeouts returned empty string")
	}
}
