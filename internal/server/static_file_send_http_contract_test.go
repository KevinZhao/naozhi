package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_FileSendsRouteOverHTTP pins the client half of the
// upload-owner divergence fix ("本机不能添加图片附件"):
//
// A WS send's uploadStore owner is frozen at WebSocket upgrade time
// (wsDeriveUploadOwner → setUploadOwner; never refreshed in no-token mode),
// while /api/sessions/upload derives its owner from the request-time nz_anon
// cookie. When the label rotates under a long-lived WS connection, a
// file-bearing WS send calls TakeAll with the stale owner and fails with
// "file not found or expired". Routing every send that carries file_ids over
// HTTP makes upload and send derive their owner from the same cookie jar on
// every request — divergence becomes structurally impossible.
func TestDashboardJS_FileSendsRouteOverHTTP(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. The WS fast path must be gated on "no files attached".
	if !strings.Contains(js, "if (wsm.isConnected() && fileIDs.length === 0) {") {
		t.Error("WS send path must be gated on fileIDs.length === 0 — file-bearing sends over WS use the upgrade-time frozen owner and fail TakeAll after nz_anon rotation")
	}

	// 2. The WS branch must NOT attach file_ids (dead code that invites a
	//    future regression back onto the diverging path).
	if strings.Contains(js, "sendMsg.file_ids") {
		t.Error("WS sendMsg must not carry file_ids — file-bearing sends belong on the HTTP path")
	}

	// 3. The HTTP path must still forward file_ids.
	if !strings.Contains(js, "payload.file_ids = fileIDs") {
		t.Error("HTTP send payload must carry file_ids — otherwise uploaded attachments are never consumed at all")
	}

	// 4. Optimistic-bubble parity: the shared helper must exist and the HTTP
	//    success path must call it while WS is connected (the live event
	//    stream is what removes .optimistic-msg; the WS-down polling path has
	//    no removal logic and must keep its legacy no-bubble behaviour).
	if !strings.Contains(js, "function renderOptimisticUserMsg(") {
		t.Fatal("renderOptimisticUserMsg helper missing — WS and HTTP send paths must share one optimistic-bubble implementation")
	}
	if !strings.Contains(js, "if (wsm.isConnected()) renderOptimisticUserMsg(text);") {
		t.Error("HTTP send success path must render the optimistic bubble when WS is connected — file-bearing sends now take this path and would otherwise lose the immediate echo the WS path always had")
	}
}
