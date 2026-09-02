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

// TestDashboardJS_HTTPSendFailureFeedback pins the #2418 follow-up fixes on
// the client side of the HTTP send path (F1/F2/F3/F6):
//
//   - F1: the server fans asynchronous HTTP-send failures out as a
//     `send_error` frame; the dashboard must dispatch it through the same
//     recovery as a WS send_ack error (toast + drop optimistic bubble +
//     roll back the running flip).
//   - F2: #msg-input is contentEditable — `input.value = text` is a silent
//     no-op, so a failed send lost the user's text. Every restore must go
//     through setMsgValue.
//   - F3: when the server says the pre-uploaded attachments were already
//     consumed (files_consumed), the client must drop its stale chips and
//     ask the user to re-attach; a blind retry would 400 with the very
//     "file not found or expired" symptom #2418 fixed.
//   - F6: 429 is not "queue full" — the server body names the limiter that
//     fired, so the client must display it instead of a hardcoded label.
func TestDashboardJS_HTTPSendFailureFeedback(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// F2
	if strings.Contains(js, "input.value = text") {
		t.Error("dashboard.js must not assign input.value on #msg-input (contentEditable) — use setMsgValue(input, text)")
	}

	// F1
	if !strings.Contains(js, "case 'send_error':") {
		t.Error("WS dispatcher must handle the send_error frame emitted for asynchronous HTTP-send failures")
	}
	if !strings.Contains(js, "onSendError(msg) {") {
		t.Error("onSendError handler missing — send_error must reuse the send_ack error recovery")
	}

	// F3
	if !strings.Contains(js, "j.files_consumed") {
		t.Error("HTTP send failure branch must read files_consumed from the error body")
	}
	if !strings.Contains(js, "if (filesConsumed) {\n        clearPendingFiles();") {
		t.Error("HTTP send failure branch must clearPendingFiles() when the server reports files_consumed")
	}

	// F6
	if strings.Contains(js, "showToast('消息队列已满，请稍后重试'") {
		t.Error("HTTP 429 must display the server's limiter-specific label, not a hardcoded queue-full message")
	}
}
