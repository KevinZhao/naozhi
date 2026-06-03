// Package api declares the unifying contract the four parallel event-storage
// layers (cli ring, eventlog/persist spool, history/naozhilog replay,
// history/merged) are converging on. R20260602-091302-ARCH-2 (#1570):
// today internal/session/eventlog_bridge.go hard-codes each backend's
// constructor and each layer carries its own append/read/subscribe
// primitives despite being semantically isomorphic. A new backend cannot be
// registry-injected; cross-tier consistency is maintained by hand.
//
// This package is the first, behaviour-free slice of the fix: it publishes
// the target interfaces (Appender / Reader / Subscriber, composed into
// EventStore) so the four backends have a single contract to implement and
// the bridge has a name to converge on, WITHOUT forcing the
// interface-everywhere refactor that would regress the per-tier performance
// hot paths (R215/R228/R240-PERF pooling). Nothing imports this package yet;
// adopting it is staged behind the bench evals the issue requires.
//
// The contract is expressed in terms of cli.EventEntry — the canonical
// in-flight unit every tier already speaks — and reuses cli.HistorySource
// for the read side so the two definitions cannot drift (the same
// drift-prevention rationale as history.Source's alias, R246-ARCH-1 #761).
// Importing cli here is cycle-free: cli is downstream of schema and does not
// import this package.
package api

import (
	"github.com/naozhi/naozhi/internal/cli"
)

// Appender is the write side of an event store. Append enqueues a single
// event; AppendBatch enqueues several atomically with respect to ordering.
// Implementations MUST NOT block the caller on durable I/O — the canonical
// cli.EventLog.Append contract is "never stall the producer", and any
// registry-injected backend is held to the same guarantee.
type Appender interface {
	Append(e cli.EventEntry)
	AppendBatch(entries []cli.EventEntry)
}

// Reader is the historical read side. It is exactly cli.HistorySource
// (LoadBefore: up to `limit` entries strictly older than beforeMS, oldest →
// newest). Reusing the canonical interface keeps the read contract identical
// across the in-memory ring and the durable tiers so their results
// concatenate without an ordering adapter.
type Reader = cli.HistorySource

// Subscriber is the change-notification side. SubscribeNew returns a typed
// EventSubscription that bundles the notify channel with its cancel func;
// the channel fires (non-blocking) on every Append and is closed by Cancel
// or by the store's own teardown — callers MUST NOT close it themselves.
// The method name matches cli.EventLog.SubscribeNew so the canonical ring
// backend satisfies this interface without a shim (R246-ARCH-12 #792 made
// SubscribeNew the typed, package-encapsulated entry point).
type Subscriber interface {
	SubscribeNew() cli.EventSubscription
}

// EventStore is the unified backend contract the four isomorphic layers
// converge on. A registry can hand a session layer any EventStore without
// the session knowing which concrete backend (claude / kiro / future) it
// is talking to — the explicit goal of #1570. The bridge's only remaining
// backend-specific responsibility becomes the EventEntry⇄persist.Entry
// conversion, not constructor selection.
type EventStore interface {
	Appender
	Reader
	Subscriber
}
