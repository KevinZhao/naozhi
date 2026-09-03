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
// the after-millisecond itself (#2432) via cli.SinceInclusive — shared with
// the HTTP poll fallback and the relay fetch_events RPC (#2456) so the three
// catch-up reads cannot drift apart again.
func entriesSinceReconnect(sess *session.ManagedSession, after int64) []cli.EventEntry {
	return sess.EventEntriesSince(cli.SinceInclusive(after))
}

// emptyInitialHistoryWanted reports whether a subscribe that found no entries
// must still receive an empty Initial history frame. Every initial subscribe
// (after==0) gets one (#2432: the dashboard renders its "暂无事件" placeholder
// only from the Initial branch, so a stub / cleared-history session was left
// blank). The running clause keeps the pre-#2432 semantics: a running session
// also gets the empty frame on reconnect (after>0); the frontend has
// _initialSubscribe=false there, so it is handled as an incremental frame
// (placeholder is not re-rendered). Non-running reconnects with nothing new
// stay silent so an incremental empty frame never wipes an existing placeholder.
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
