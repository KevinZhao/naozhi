package upstream

// reqSem observability. These expvars live here rather than in
// internal/metrics because they are coupled to the connector's reqSem
// primitive; expvar self-registers on /debug/vars so no server wiring is needed.

import "expvar"

// reqSemReqInflight is a gauge (Add(+1)/Add(-1) around acquire/release) of
// reverse-RPC requests currently holding a reqSem slot. A sustained value near
// capacity (16) means the primary dispatches faster than handleRequest retires
// requests.
var reqSemReqInflight = expvar.NewInt("naozhi_upstream_reqsem_inflight")

// reqSemReqWaitTotal counts reverse-RPC requests that missed the non-blocking
// reqSem acquire and had to block. Its rate relative to total requests is the
// saturation ratio; a sustained few percent means raise capacity or find the
// slow handleRequest path (typically sess.Send blocked on the CLI watchdog).
var reqSemReqWaitTotal = expvar.NewInt("naozhi_upstream_reqsem_wait_total")

// connectorBackoffMillis is a gauge (Set, not Add) of the connector's current
// reconnect backoff in milliseconds: a steady 1000 means healthy reconnects; a
// value pinned at circuitBreakerBackoff means the breaker tripped. naozhi runs
// one Connector per process, so a process-global gauge is unambiguous.
var connectorBackoffMillis = expvar.NewInt("naozhi_upstream_connector_backoff_millis")
