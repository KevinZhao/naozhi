package dispatch

import "expvar"

// Process-wide expvar counters that mirror the per-Dispatcher atomic
// counters so /debug/vars surfaces them without requiring callers to
// scrape /health. R245-ARCH-36 (#892): dispatch was the only subsystem
// keeping its operational counters off expvar; with multiple Dispatcher
// instances (Dashboard + IM) sharing the same process the per-instance
// .Metrics() snapshots couldn't be aggregated externally.
//
// These vars live in the dispatch package (not internal/metrics) to
// keep the wiring local to the call sites and avoid forcing
// internal/metrics to take a build-time dependency on dispatch types.
// The naming scheme matches the metrics package convention
// (naozhi_<area>_<event>_total) so /debug/vars consumers see a flat,
// homogenous list. A future consolidation can move these constants
// into internal/metrics with no call-site change — only the var
// declaration moves.
//
// Counters are monotonic since process start. They are NOT
// per-Dispatcher; if two Dispatcher instances share a process (today
// IM-side dispatcher only, dashboard goes through server.* not
// dispatch), both contribute to the same expvar value.
var (
	// dispatchMessageTotal counts non-slash IM messages accepted by
	// BuildHandler / sendAndReply. Mirrors Dispatcher.messageCount
	// (per-instance) so operators can see traffic without scraping
	// /health.
	dispatchMessageTotal = expvar.NewInt("naozhi_dispatch_message_total")

	// dispatchReplyErrorTotal counts errors returned by
	// Capabilities.Send during sendAndReply (includes timeouts /
	// ErrSessionReset). Pairs with dispatchSendFailTotal — a rising
	// delta here means Claude returned an error but the platform
	// reply infrastructure was healthy.
	dispatchReplyErrorTotal = expvar.NewInt("naozhi_dispatch_reply_error_total")

	// dispatchSendFailTotal counts user-visible reply failures (the
	// platform adapter returned a non-nil error from Reply /
	// EditMessage). A growing delta is operator-actionable: Claude
	// is processing messages but replies are not reaching the IM
	// channel.
	dispatchSendFailTotal = expvar.NewInt("naozhi_dispatch_send_fail_total")
)
