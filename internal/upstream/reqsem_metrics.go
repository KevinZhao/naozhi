package upstream

// reqSem observability (R220-PERF-P2).
//
// These counters live in the upstream package rather than in
// internal/metrics because they are tightly coupled to the connector's
// reqSem concurrency primitive — nothing else in naozhi touches them,
// and no test outside this package has a reason to read them. Keeping
// the declaration local:
//
//   - avoids an import from upstream → metrics (only for these two
//     vars), matching the "accept interfaces, return structs" and
//     small-dependency guidance in the Go rules;
//   - lets a reader spot the acquire/release pair and the counter
//     definitions in the same package without cross-tree jumps;
//   - makes it trivial to change the implementation (expvar → prom
//     gauge) by editing one file, since the call sites are right next
//     door in connector.go.
//
// /debug/vars publishes every expvar.NewInt — the stdlib package
// registers itself — so these vars show up on the dashboard's
// existing expvar endpoint without any wiring in server/.

import "expvar"

// reqSemReqInflight is a gauge (not a counter) exposing the current
// number of reverse-RPC requests holding the connector reqSem slot.
// Implemented as *expvar.Int via Add(+1)/Add(-1) around the
// acquire/release pair — /debug/vars consumers read a point-in-time
// value, not a cumulative total. A sustained value at or near the
// reqSem capacity is the primary signal that primary is dispatching
// requests faster than handleRequest can retire them, and the
// earliest hint that the capacity (default 16) may be binding.
// Pairs with reqSemReqWaitTotal: inflight shows the instantaneous
// load, WaitTotal shows whether any request actually had to block
// for a slot.
var reqSemReqInflight = expvar.NewInt("naozhi_upstream_reqsem_inflight")

// reqSemReqWaitTotal counts reverse-RPC requests that could NOT
// acquire reqSem on the first non-blocking attempt and had to fall
// through to a blocking select{acquire, ctx.Done}. Monotonic.
// A zero delta under load means the semaphore capacity is
// comfortable; a non-zero delta means requests are being serialized
// behind the cap. Delta rate relative to the total request rate is
// the saturation ratio — if it climbs past a few percent of total
// requests sustained, raise reqSem capacity or investigate slow
// handleRequest paths (typically sess.Send blocked on the CLI
// watchdog, see R51-REL-005).
var reqSemReqWaitTotal = expvar.NewInt("naozhi_upstream_reqsem_wait_total")
