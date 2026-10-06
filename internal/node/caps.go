package node

import (
	"errors"
	"log/slog"
	"sort"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// CapSubscribeHistory is the node tag for answering a want_history subscribe
// with its own opening page (ReverseMsg.WantHistory); a primary that sees it
// skips the parallel fetch_events for that page.
const CapSubscribeHistory = "subscribe-history"

// CapSendStatus is the hub tag for reading the "send" RPC's {"status"} result
// (ReverseConn.Send). A primary without it drops the result and reports every
// send that returned no error as accepted, so a node answers it ErrSendBusy
// instead of a "busy" status.
const CapSendStatus = "send-status"

// ErrSendBusy is the refusal for a send that was not buffered: the session is
// busy and its queue is disabled, or the node is shutting down.
var ErrSendBusy = errors.New("会话正忙，消息未送达，请稍后重试")

// knownServerCaps is the capability set this binary understands. Unknown
// advertised caps only WARN (mixed-version signal); the node still registers.
// Add here when a new capability is introduced on the client side.
var knownServerCaps = map[string]struct{}{
	clievent.SchemaCap: {},
	"gemini":           {},
	"acp":              {},
	"codex-app-server": {},
	"askuser":          {},
	"attach":           {},
	"scratch":          {},

	CapSubscribeHistory: {},
}

// HubCaps is what the hub advertises about itself on the registered ack: the
// EventEntry schema tag and CapSendStatus, facts the node cannot discover any
// other way. The backend entries in knownServerCaps are the node's side of the
// negotiation and do not belong here.
func HubCaps() []string {
	return []string{clievent.SchemaCap, CapSendStatus}
}

// logUnknownCaps WARNs when advertised contains caps outside knownServerCaps.
func logUnknownCaps(nodeID string, advertised []string) {
	if len(advertised) == 0 {
		return
	}
	var unknown []string
	for _, c := range advertised {
		if _, ok := knownServerCaps[c]; !ok {
			unknown = append(unknown, c)
		}
	}
	if len(unknown) == 0 {
		return
	}
	sort.Strings(unknown)
	slog.Warn("reverse node advertised unknown capabilities",
		"node_id", nodeID,
		"unknown_caps", unknown,
		"hint", "node binary may be newer than naozhi; update naozhi or strip unknown caps on client side")
}
