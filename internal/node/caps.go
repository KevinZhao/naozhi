package node

import (
	"log/slog"
	"sort"
)

// knownServerCaps is the capability set this binary understands. Unknown
// advertised caps only WARN (mixed-version signal); the node still registers.
// Add here when a new capability is introduced on the client side.
var knownServerCaps = map[string]struct{}{
	"gemini":           {},
	"acp":              {},
	"codex-app-server": {},
	"askuser":          {},
	"attach":           {},
	"scratch":          {},
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
