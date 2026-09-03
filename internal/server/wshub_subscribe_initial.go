// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     none
//	READS:      none; pure helpers for completeSubscribe (wshub_subscribe.go)
package server

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// entriesSinceReconnect is the `subscribe{after}` catch-up read. It re-admits
// the after-millisecond itself (#2432): EntriesSince is strictly greater-than,
// so a same-ms sibling appended after the client's last delivery would never
// be replayed. The dashboard dedups same-ms replays by uuid (onHistory /
// onCronLiveHistory), so the over-inclusive read is safe.
func entriesSinceReconnect(sess *session.ManagedSession, after int64) []cli.EventEntry {
	return sess.EventEntriesSince(after - 1)
}

// emptyInitialHistoryWanted reports whether a subscribe that found no entries
// must still receive an empty Initial history frame. Every initial subscribe
// (after==0) gets one (#2432: the dashboard renders its "暂无事件" placeholder
// only from the Initial branch, so a stub / cleared-history session was left
// blank); running sessions additionally get it on reconnect so the pane flips
// to its loading placeholder. Reconnects with nothing new otherwise stay
// silent so an incremental empty frame never wipes an existing placeholder.
func emptyInitialHistoryWanted(msg node.ClientMsg, state string) bool {
	return msg.After == 0 || state == "running"
}

// nonNilEntries guarantees the frame serialises as `events: []`, never null.
func nonNilEntries(entries []cli.EventEntry) []cli.EventEntry {
	if entries == nil {
		return []cli.EventEntry{}
	}
	return entries
}
