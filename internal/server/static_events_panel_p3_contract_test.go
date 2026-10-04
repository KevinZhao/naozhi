// static_events_panel_p3_contract_test.go — wiring pins for the #2430 P3 items
// on the session events panel and the #2432 WS protocol reconciliation.
// dashboard.js has no JS unit runner, so these source-greps lock the shape the
// Playwright regressions (test/e2e/events_panel_p3.test.js) exercise:
//
//  4. prependEvents drops the old leading time divider when the newest
//     prepended bubble is within EVENT_DIVIDER_GAP_MS of it (pagination seam
//     used to show two stacked dividers).
//  5. appendEvents (WS-down poll path) replaces the optimistic bubble on the
//     first arrival of the real `user` event and dedups user replays by uuid,
//     mirroring onEvent/onHistory — a send that left over WS and was echoed
//     by the poll rendered twice.
//  6. onSendAck busy/error removes the bubble stamped with THIS send's id
//     (send frame `id`, echoed on send_ack) instead of the first
//     .optimistic-msg in the DOM.
//     B. `unsubscribed` is an explicit no-op case; the node-disconnect broadcast
//     deselects the selected session when it lived on the dead node.
//     C. a keyed "history unavailable" error for the selected remote session
//     mounts a retry that subscribes again (#3005).
package server

import (
	"strings"
	"testing"
)

func TestDashboardJS_SendAckRollsBackOwnBubbleById(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	// Send frame id is stamped on the optimistic bubble.
	if !strings.Contains(js, "renderOptimisticUserMsg(text, id);") {
		t.Error("WS send path must pass the send frame id to renderOptimisticUserMsg")
	}
	ren := jsFuncBody(t, js, "renderOptimisticUserMsg")
	if !strings.Contains(ren, "if (sendId) el.lastElementChild.setAttribute('data-send-id', sendId);") {
		t.Error("renderOptimisticUserMsg must stamp data-send-id on the bubble")
	}

	rm := jsFuncBody(t, js, "removeOptimisticMsg")
	if !strings.Contains(rm, `'.optimistic-msg[data-send-id="' + sel + '"]'`) {
		t.Error("removeOptimisticMsg must select the bubble by data-send-id when an id is given")
	}
	if !strings.Contains(rm, "opt = root.querySelector('.optimistic-msg');") {
		t.Error("removeOptimisticMsg must keep the legacy first-bubble fallback for id-less acks (send_error / old servers)")
	}

	ack := jsMethodBody(t, js, "onSendAck")
	for _, status := range []string{"'busy'", "'error'"} {
		idx := strings.Index(ack, "msg.status === "+status)
		if idx < 0 {
			t.Fatalf("onSendAck missing %s branch", status)
		}
		seg := ack[idx:]
		if end := strings.Index(seg, "} else if"); end > 0 {
			seg = seg[:end]
		}
		if !strings.Contains(seg, "removeOptimisticMsg(msg.id);") {
			t.Errorf("onSendAck %s branch must remove the bubble of THIS send (removeOptimisticMsg(msg.id))", status)
		}
		if strings.Contains(seg, "document.querySelector('.optimistic-msg')") {
			t.Errorf("onSendAck %s branch still removes the first .optimistic-msg in the DOM regardless of send id", status)
		}
	}
}

func TestDashboardJS_UnsubscribedFrameIsExplicitNoop(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	// A documented no-op: a handler with no parameter and an empty body, and
	// no claim that could act on the frame instead.
	if got := wsOnHandler(t, js, "unsubscribed", false); got != "wsm.on(NZ_CONTRACT.WS.unsubscribed, () => {})" {
		t.Errorf("unsubscribed must be a bare documented no-op (wsm.unsubscribe already reset the client state), got %q", got)
	}
	if n := strings.Count(js, "wsm.on(NZ_CONTRACT.WS.unsubscribed,"); n != 1 {
		t.Errorf("unsubscribed has %d registrations, want the one no-op", n)
	}
}

func TestDashboardJS_NodeDisconnectDeselectsStaleSession(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	body := wsOnHandler(t, js, "error", false)
	idx := strings.Index(body, "if (!msg.key && msg.node && msg.error === 'node disconnected')")
	if idx < 0 {
		t.Fatal("node-disconnected branch missing")
	}
	branch := body[idx:]
	if end := strings.Index(branch, "reconcileSelectedNode();"); end > 0 {
		branch = branch[:end]
	} else {
		t.Fatal("node-disconnected branch must still call reconcileSelectedNode()")
	}
	// Ownership must come from the session store (sessionsData / perSession.nodes
	// keyed by the dead node), never from selectedNode: that global is the
	// dispatch target and wireNodePicker rewrites it on the new-session
	// picker's change event, so a local session with the picker on n1 would
	// otherwise be wiped when n1 disconnects. Pending sessions stay alone.
	if !strings.Contains(branch, "(sessionList.sessionsData[sid(selection.key, msg.node)] || perSession.nodes[selection.key] === msg.node)") {
		t.Error("node-disconnected branch must resolve the selected session's node from sessionList.sessionsData/perSession.nodes, not selection.node")
	}
	if strings.Contains(branch, "selection.node === msg.node") {
		t.Error("node-disconnected branch must not infer session ownership from selection.node (dispatch target, rewritten by wireNodePicker)")
	}
	if !strings.Contains(branch, "perSession.workspaces[selection.key] === undefined") {
		t.Error("node-disconnected branch must skip pending (never-sent) sessions")
	}
	if !strings.Contains(branch, "deselectNodeSession(msg.node);") {
		t.Error("node-disconnected branch must call deselectNodeSession")
	}
	fn := jsFuncBody(t, js, "deselectNodeSession")
	// deselectNodeSession lives in system_view.js, which reads dashboard state
	// from the state.js objects and imports its helpers.
	for _, want := range []string{
		"if (draft) perSession.drafts[selection.key] = draft;", // keep the operator's text
		"selection.key = null;",
		"main.innerHTML = mainEmptyHtml();",
		"wireQuickAskInput();",
		"已断开",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("deselectNodeSession missing %q", want)
		}
	}
	for _, forbid := range []string{"removePendingSession", "delete perSession.workspaces", "fetch(", "DELETE"} {
		if strings.Contains(fn, forbid) {
			t.Errorf("deselectNodeSession must not touch pending sessions or the backend (%q)", forbid)
		}
	}
}

func TestDashboardJS_HistoryUnavailableOffersRetry(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	body := wsOnHandler(t, js, "error", false)
	// Only the selected session's own failure: same key AND same node.
	if !strings.Contains(body, "if (msg.error === 'history unavailable' && msg.key === selection.key && msg.node === selection.node) showHistoryRetry();") {
		t.Error("error handler must mount the history retry for the selected session's keyed 'history unavailable'")
	}
	fn := jsFuncBody(t, js, "showHistoryRetry")
	for _, want := range []string{
		"if (el && !el.querySelector('.event')) el.replaceChildren();", // a blank or placeholder pane shows only the retry
		"btn.remove();",
		"if (sessionStream._initialSubscribe) sessionStream.lastEventTimeWs = 0;", // a retry before the opening page asks for it again
		"sessionStream.subscribe(selection.key, selection.node);",
		"历史记录加载失败",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("showHistoryRetry missing %q", want)
		}
	}
}
