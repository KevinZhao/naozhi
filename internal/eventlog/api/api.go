// Package api declares the unifying contract for the four parallel
// event-storage layers (cli ring, eventlog/persist spool, history/naozhilog
// replay, history/merged): Appender / Reader / Subscriber, composed into
// EventStore, so a backend can be registry-injected instead of hard-coded
// in internal/session/eventlog_bridge.go (#1570). Nothing imports this
// package yet; adoption is staged behind bench evals so the per-tier
// pooling hot paths are not regressed.
//
// The contract is expressed in clievent.EventEntry and reuses
// cli.HistorySource for the read side so the two cannot drift. Importing
// cli here is cycle-free: cli does not import this package.
package api

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Appender is the write side. Append enqueues one event; AppendBatch
// enqueues several, ordered atomically. Implementations MUST NOT block the
// caller on durable I/O (the cli.EventLog.Append "never stall" contract).
type Appender interface {
	Append(e clievent.EventEntry)
	AppendBatch(entries []clievent.EventEntry)
}

// Reader is the historical read side: exactly cli.HistorySource, so results
// from the ring and the durable tiers concatenate without an adapter.
type Reader = cli.HistorySource

// Subscriber is the change-notification side. SubscribeNew returns an
// EventSubscription bundling the notify channel with its cancel func; the
// channel fires (non-blocking) on every Append and is closed by Cancel or
// store teardown — callers MUST NOT close it. The name matches
// cli.EventLog.SubscribeNew so the ring backend satisfies it without a shim.
type Subscriber interface {
	SubscribeNew() cli.EventSubscription
}

// EventStore is the unified backend contract (#1570): a registry can hand a
// session any EventStore without it knowing the concrete backend; the
// bridge only keeps the EventEntry⇄persist.Entry conversion.
type EventStore interface {
	Appender
	Reader
	Subscriber
}
