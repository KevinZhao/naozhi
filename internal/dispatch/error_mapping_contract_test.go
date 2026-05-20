package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/usermsg"
)

// TestDispatch_R215_CR_P2_1_ContextErrorMapping locks in the parity between
// dispatch.go and the dashboard send path for context.Canceled /
// context.DeadlineExceeded — both surfaces must yield the "系统正在重启"
// hint instead of the generic /new reset prompt.
//
// History: dispatch.go originally lacked this case while
// errors_usermsg.go had it, so an IM user saw "处理失败" while a dashboard
// user saw "系统正在重启" for the same shutdown event. R215-CR-P2-1.
//
// R226-CR-9 collapsed the duplicated switch statements onto
// internal/usermsg.ForSendError. Both surfaces now exercise the helper
// and so this contract test asserts the helper behaviour directly
// instead of grepping dispatch.go's source.
func TestDispatch_R215_CR_P2_1_ContextErrorMapping(t *testing.T) {
	const want = "系统正在重启"
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		got := usermsg.ForSendError(err, "")
		if !strings.Contains(got, want) {
			t.Errorf("usermsg.ForSendError(%v) = %q, missing required fragment %q (R215-CR-P2-1 regressed)", err, got, want)
		}
	}
}
