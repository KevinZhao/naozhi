package node

import (
	"log/slog"
	"sort"
)

// knownServerCaps is the set of capability strings this naozhi binary
// understands. Remote nodes advertising capabilities OUTSIDE this set
// trigger a WARN log — the server still registers them normally (to
// preserve forward-compat with newer node binaries), but the WARN gives
// operators early signal that a mixed-version deployment is running.
// Add to this set when a new capability is introduced on the client side.
//
// R212-ARCH-402.
var knownServerCaps = map[string]struct{}{
	"gemini":  {},
	"acp":     {},
	"askuser": {},
	"attach":  {},
	"scratch": {},
}

// logUnknownCaps emits a WARN when `advertised` contains cap strings not
// in knownServerCaps. No-op when empty or all-known. Safe to call under
// any lock — only stdlib slog.
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
