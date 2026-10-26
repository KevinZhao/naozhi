package node

import "testing"

// TestErrSendBusyText pins the busy refusal: primaries without CapSendStatus
// show this string verbatim.
func TestErrSendBusyText(t *testing.T) {
	const want = "会话正忙，消息未送达，请稍后重试"
	if got := ErrSendBusy.Error(); got != want {
		t.Errorf("ErrSendBusy = %q, want %q", got, want)
	}
}
