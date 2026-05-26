package usermsg

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// TestForSendError_ContractTable is the canonical sentinel→user-text
// mapping contract for ForSendError. R245-CR-004 (#874): without this
// table-driven assertion, new sentinels added to internal/cli or
// internal/session silently fall through to the generic /new hint.
//
// Every sentinel listed here has a dedicated branch in usermsg.go.
// When adding a new sentinel:
//  1. Add a `case errors.Is(err, ...)` branch in ForSendError above.
//  2. Add a row here.
//  3. If the new sentinel needs path-specific phrasing (e.g. timeout
//     duration), also wire it through UserMessage and add a row to
//     TestUserMessage_TimeoutSpecialisation.
//
// Removing a sentinel from this table without removing the
// corresponding branch — or vice versa — is the regression this test
// is designed to catch.
func TestForSendError_ContractTable(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		key      string
		wantSubs []string
		notSubs  []string
	}{
		{
			name:     "nil err returns empty",
			err:      nil,
			wantSubs: []string{""},
		},
		{
			name:     "ErrMaxProcs",
			err:      session.ErrMaxProcs,
			wantSubs: []string{"处理已满"},
		},
		{
			name:     "ErrMaxExemptSessions",
			err:      session.ErrMaxExemptSessions,
			wantSubs: []string{"长时会话", "planner/cron"},
		},
		{
			name:     "ErrNoCLIWrapper",
			err:      session.ErrNoCLIWrapper,
			wantSubs: []string{"会话后端未配置"},
		},
		{
			name:     "ErrNoActiveProcess regular key",
			err:      session.ErrNoActiveProcess,
			key:      "feishu:p2p:u_x:agent",
			wantSubs: []string{"会话已休眠"},
			notSubs:  []string{"定时任务"},
		},
		{
			name:     "ErrNoActiveProcess cron key",
			err:      session.ErrNoActiveProcess,
			key:      "cron:slug",
			wantSubs: []string{"定时任务", "下一次触发"},
		},
		{
			name:     "ErrNoOutputTimeout",
			err:      cli.ErrNoOutputTimeout,
			wantSubs: []string{"处理超时"},
		},
		{
			name:     "ErrTotalTimeout",
			err:      cli.ErrTotalTimeout,
			wantSubs: []string{"处理超时"},
		},
		{
			name:     "ErrProcessExited",
			err:      cli.ErrProcessExited,
			wantSubs: []string{"进程意外退出"},
		},
		{
			name:     "ErrAbortedByUrgent",
			err:      cli.ErrAbortedByUrgent,
			wantSubs: []string{"/urgent", "打断"},
		},
		{
			name:     "ErrReconnectedUnknown",
			err:      cli.ErrReconnectedUnknown,
			wantSubs: []string{"系统已重启", "状态未知"},
		},
		{
			name:     "ErrSessionReset",
			err:      cli.ErrSessionReset,
			wantSubs: []string{"会话已重置"},
		},
		{
			name:     "ErrTooManyPending",
			err:      cli.ErrTooManyPending,
			wantSubs: []string{"排队已满", "/stop"},
		},
		{
			name:     "ErrProcessBusy",
			err:      cli.ErrProcessBusy,
			wantSubs: []string{"正在处理上一条消息"},
		},
		{
			name:     "ErrMessageTooLarge",
			err:      cli.ErrMessageTooLarge,
			wantSubs: []string{"消息内容过大"},
		},
		{
			name:     "ErrOrphanedSlot",
			err:      cli.ErrOrphanedSlot,
			wantSubs: []string{"处理超时"},
		},
		{
			name:     "context.Canceled",
			err:      context.Canceled,
			wantSubs: []string{"系统正在重启"},
		},
		{
			name:     "context.DeadlineExceeded",
			err:      context.DeadlineExceeded,
			wantSubs: []string{"系统正在重启"},
		},
		{
			name:     "wrapped sentinel still matches via errors.Is",
			err:      wrapErr(session.ErrMaxProcs),
			wantSubs: []string{"处理已满"},
		},
		{
			name:     "unknown error falls through to /new hint",
			err:      errors.New("totally unknown failure"),
			wantSubs: []string{"/new"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ForSendError(tt.err, tt.key)
			if tt.err == nil {
				if got != "" {
					t.Fatalf("ForSendError(nil) = %q, want empty string", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("ForSendError(%v) returned empty string; every mapped sentinel must yield user-visible text", tt.err)
			}
			for _, want := range tt.wantSubs {
				if want == "" {
					continue
				}
				if !strings.Contains(got, want) {
					t.Errorf("ForSendError(%v, %q) = %q, missing required fragment %q", tt.err, tt.key, got, want)
				}
			}
			for _, bad := range tt.notSubs {
				if strings.Contains(got, bad) {
					t.Errorf("ForSendError(%v, %q) = %q, must not contain %q", tt.err, tt.key, got, bad)
				}
			}
		})
	}
}

// wrapErr exercises the errors.Is unwrapping path so a refactor that
// accidentally switches to == comparison instead of errors.Is breaks
// this test loudly.
func wrapErr(err error) error {
	return &wrappedErr{inner: err}
}

type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }
