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
package server

import (
	"strings"
	"testing"
)

func TestDashboardJS_PrependDropsRedundantSeamDivider(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	body := jsFuncBody(t, js, "prependEvents")
	for _, want := range []string{
		"const oldLeadDivider = leadingTimeDivider(el);",
		"const newestPrependedTime = lastDividerTime(frag);",
		"if (leadT && leadT - newestPrependedTime < EVENT_DIVIDER_GAP_MS) oldLeadDivider.remove();",
		// Found by the seam e2e: inserting before a moving el.firstChild
		// reversed the prepended page; anchor once.
		"const anchor = el.firstChild;",
		"el.insertBefore(frag.firstChild, anchor);",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prependEvents missing %q — the old prevTime=0 divider stays stacked under the prepended page", want)
		}
	}
	// The removal must run before the bottom-anchored scroll restore so the
	// height change is accounted for.
	rm := strings.Index(body, "oldLeadDivider.remove();")
	restore := strings.Index(body, "el.scrollTop = el.scrollHeight - el.clientHeight - prevScrollFromBottom;")
	if rm < 0 || restore < 0 || rm > restore {
		t.Error("redundant divider removal must precede the scrollTop restore in prependEvents")
	}
	lead := jsFuncBody(t, js, "leadingTimeDivider")
	if !strings.Contains(lead, "if (c.classList.contains('event')) return null;") {
		t.Error("leadingTimeDivider must stop at the first bubble — only a divider that precedes every bubble is the prevTime=0 one")
	}
}

func TestDashboardJS_AppendEventsReplacesOptimisticBubble(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	body := jsFuncBody(t, js, "appendEvents")
	userIdx := strings.Index(body, "if (e.type === 'user') {")
	if userIdx < 0 {
		t.Fatal("appendEvents must have a user-event branch like onHistory")
	}
	branch := body[userIdx:]
	if end := strings.Index(branch, "const h = eventHtml(e);"); end > 0 {
		branch = branch[:end]
	}
	if !strings.Contains(branch, "if (eventAlreadyRendered(el, e.uuid)) {") {
		t.Error("appendEvents user branch must dedup replays by uuid (same rule as onHistory)")
	}
	if !strings.Contains(branch, "const opt = el.querySelector('.optimistic-msg');\n      if (opt) opt.remove();") {
		t.Error("appendEvents must remove the optimistic bubble when the real user event arrives via poll (#2430 double bubble after WS drop)")
	}
	// Only a duplicate (uuid already on screen) may be skipped — never a real
	// event, and the skip must still advance the time cursor.
	if !strings.Contains(branch, "if (e.time && e.time > lastRenderedEventTime) lastRenderedEventTime = e.time;\n        return;") {
		t.Error("appendEvents uuid-dedup skip must advance lastRenderedEventTime before returning")
	}
	if !strings.Contains(branch, "lockRenderedAskCards(el);") {
		t.Error("appendEvents user branch must keep locking ask cards (P2 #2430 item 3)")
	}
}

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
	body := wsOnMessageBody(t, readDashboardJS(t))
	idx := strings.Index(body, "case 'unsubscribed':")
	if idx < 0 {
		t.Fatal("wsm.onMessage must have an explicit `case 'unsubscribed':` — the hub emits it from three sites in wshub_subscribe.go")
	}
	seg := body[idx:]
	if next := strings.Index(seg, "case '"); next > 0 {
		if n2 := strings.Index(seg[next:], "case '"); n2 > 0 {
			seg = seg[:next+n2]
		}
	}
	// A documented no-op: comment + break, no state mutation.
	if !strings.Contains(seg, "break;") || strings.Contains(seg[:strings.Index(seg, "break;")], "this.") {
		t.Error("unsubscribed case must be a bare documented no-op (wsm.unsubscribe already reset the client state)")
	}
}

func TestDashboardJS_NodeDisconnectDeselectsStaleSession(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	body := wsOnMessageBody(t, js)
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
	// Must compare against the dead node BEFORE reconcileSelectedNode snaps
	// selectedNode back to local, and must leave pending sessions alone.
	if !strings.Contains(branch, "if (selectedKey && selectedNode === msg.node && sessionWorkspaces[selectedKey] === undefined) {") {
		t.Error("node-disconnected branch must deselect the selected session only when it lived on that node and is not a pending (never-sent) session")
	}
	if !strings.Contains(branch, "deselectNodeSession(msg.node);") {
		t.Error("node-disconnected branch must call deselectNodeSession")
	}
	fn := jsFuncBody(t, js, "deselectNodeSession")
	for _, want := range []string{
		"if (draft) sessionDrafts[selectedKey] = draft;", // keep the operator's text
		"selectedKey = null;",
		"main.innerHTML = mainEmptyHtml();",
		"wireQuickAskInput();",
		"已断开",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("deselectNodeSession missing %q", want)
		}
	}
	for _, forbid := range []string{"removePendingSession", "delete sessionWorkspaces", "fetch(", "DELETE"} {
		if strings.Contains(fn, forbid) {
			t.Errorf("deselectNodeSession must not touch pending sessions or the backend (%q)", forbid)
		}
	}
}
