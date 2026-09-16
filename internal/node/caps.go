package node

import (
	"log/slog"
	"sort"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

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
}

// HubCaps is what the hub advertises about itself on the registered ack.
//
// Only the EventEntry schema tag: the other entries in knownServerCaps name
// backends a NODE can run, which is the node's side of the negotiation, not the
// hub's. Adding one here would tell a node the hub can run gemini, which is not
// a thing a node ever needs to know — whereas the schema tag is exactly the fact
// the node cannot discover any other way.
func HubCaps() []string {
	return []string{clievent.SchemaCap}
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
