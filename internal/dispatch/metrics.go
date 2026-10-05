package dispatch

import "expvar"

// Process-wide expvar counters mirroring the per-Dispatcher atomic counters
// so /debug/vars surfaces them without scraping /health (#892). They live in
// dispatch (not internal/metrics) to keep wiring local; naming follows the
// naozhi_<area>_<event>_total convention. Counters are monotonic since
// process start and NOT per-Dispatcher: multiple instances in one process
// contribute to the same value.
var (
	// dispatchMessageTotal counts non-slash IM messages accepted by
	// BuildHandler (mirrors Dispatcher.messageCount).
	dispatchMessageTotal = expvar.NewInt("naozhi_dispatch_message_total")

	// dispatchReplyErrorTotal counts failed sends an IM turn reported
	// (handleSendError; includes timeouts / ErrSessionReset): Claude
	// errored but the platform reply path was healthy.
	dispatchReplyErrorTotal = expvar.NewInt("naozhi_dispatch_reply_error_total")

	// dispatchSendFailTotal counts user-visible reply failures (platform
	// adapter Reply / EditMessage returned an error): replies are not
	// reaching the IM channel.
	dispatchSendFailTotal = expvar.NewInt("naozhi_dispatch_send_fail_total")

	// dispatchTurnErrorResultTotal counts IM turns whose result was a
	// failure, keyed by usermsg's turn class ("error_text" for an is_error
	// answer that has text). Delivery still counts as a reply success.
	dispatchTurnErrorResultTotal = expvar.NewMap("naozhi_dispatch_turn_error_result_total")

	// dispatchDeniedTotal counts IM messages the access policy refused,
	// keyed "<platform>:<reason>" (imauth.Reason*).
	dispatchDeniedTotal = expvar.NewMap("naozhi_dispatch_denied_total")
)
