package cli_test

import (
	"fmt"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/usermsg"
)

// The user text for a CLI that exits during Init is the one its stderr
// class names, on the error Spawn really returns through the router's wrap.
func TestInitHandshakeExit_UserText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{"auth exit", `{"type":"cli_exited","code":1,"stderr_tail":["Error: authentication failed"]}`, "后端认证失败，请联系管理员。"},
		{"exit 0", `{"type":"cli_exited","code":0,"stderr_tail":["Error: authentication failed"]}`, "进程意外退出，请重新发送消息，系统会自动重启会话。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := fmt.Errorf("session k: spawn process: %w", cli.InitHandshakeErrForTest(t, tc.frame))
			if got := usermsg.ForSendError(err, ""); got != tc.want {
				t.Errorf("ForSendError(%v) = %q, want %q", err, got, tc.want)
			}
		})
	}
}
