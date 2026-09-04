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
	// BuildHandler / sendAndReply (mirrors Dispatcher.messageCount).
	dispatchMessageTotal = expvar.NewInt("naozhi_dispatch_message_total")

	// dispatchReplyErrorTotal counts errors returned by Capabilities.Send
	// during sendAndReply (includes timeouts / ErrSessionReset): Claude
	// errored but the platform reply path was healthy.
	dispatchReplyErrorTotal = expvar.NewInt("naozhi_dispatch_reply_error_total")

	// dispatchSendFailTotal counts user-visible reply failures (platform
	// adapter Reply / EditMessage returned an error): replies are not
	// reaching the IM channel.
	dispatchSendFailTotal = expvar.NewInt("naozhi_dispatch_send_fail_total")
)
