package dispatch

import (
	"os"
	"strings"
	"testing"
)

// TestDispatch_R215_CR_P2_1_ContextErrorMapping locks in the parity between
// dispatch.go and server/errors_usermsg.go for context.Canceled /
// context.DeadlineExceeded — both surfaces must yield the "系统正在重启"
// hint instead of the generic /new reset prompt.
//
// History: dispatch.go originally lacked this case while
// errors_usermsg.go had it, so an IM user saw "处理失败" while a dashboard
// user saw "系统正在重启" for the same shutdown event. R215-CR-P2-1.
func TestDispatch_R215_CR_P2_1_ContextErrorMapping(t *testing.T) {
	src, err := os.ReadFile("dispatch.go")
	if err != nil {
		t.Fatalf("read dispatch.go: %v", err)
	}
	got := string(src)
	for _, want := range []string{
		"errors.Is(err, context.Canceled)",
		"errors.Is(err, context.DeadlineExceeded)",
		"系统正在重启",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dispatch.go missing required error-mapping fragment %q (R215-CR-P2-1 regressed)", want)
		}
	}
}
