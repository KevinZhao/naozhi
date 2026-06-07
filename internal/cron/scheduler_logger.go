package cron

// scheduler_logger.go holds the robfig/cron Printf-logger adapter split out
// of scheduler.go (move-only, #1282): slogPrintfLogger, the cronPanicMarker
// const, and the Printf method. No behaviour changed — relocated verbatim.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// slogPrintfLogger satisfies the Printf interface that robfig/cron's
// PrintfLogger expects, routing every emitted line through slog instead of
// the standard log package.
//
// Observability note: robfig/cron wraps this via non-verbose PrintfLogger
// (logger.go:28 in the vendored lib) which compiles Info() out entirely
// when logInfo=false. SkipIfStillRunning calls Info (chain.go:88) and
// therefore never reaches Printf at all; only Error() lines do — i.e.
// recover-panic recoveries and schedule parse failures. Panic recoveries
// are logged at Error (a real fault); anything else stays at Warn so
// upstream library changes that route new events through Error remain
// visible without silently demoting them.
type slogPrintfLogger struct{}

// cronPanicMarker is the substring scanned in robfig/cron-emitted log
// lines to escalate to slog.Error rather than slog.Warn. Pulled out as a
// named const (R247-CR-23) so call-site readers see WHAT we look for and
// WHY in one place — the previous inline `strings.Contains(msg, "panic")`
// read as a negative assertion ("if this is a panic line") that obscured
// the upstream-stability rationale baked into the comment.
//
// robfig/cron's Recover wrapper invokes logger.Error(err, "panic",
// "stack", ...) (chain.go ~line 50); the printfLogger Error formatter
// renders the msg argument verbatim, so the literal substring "panic"
// is guaranteed to appear in every recover-emitted line. No other Error
// path through the library carries this token.
//
// R249-CR-24: dropped the historical cronRecoveredMarker = "recovered"
// fallback. It existed as a forward-compat hedge for a hypothetical
// upstream rename of the Recover message but never matched real output:
// robfig/cron 3.0.x emits "panic" only, and a future rename would arrive
// in a Go module bump where we'd update the marker alongside any other
// breakage. Single Contains scan is enough — keeping a no-op fallback
// added a strings.Contains call per emitted line for no observed signal.
const cronPanicMarker = "panic"

func (slogPrintfLogger) Printf(format string, args ...any) {
	// R249-PERF-10 (#931): gate on slog.Enabled before paying for
	// fmt.Sprintf + strings.TrimRight + strings.Contains. Every line this
	// logger emits lands at Warn or Error (see type godoc + cronPanicMarker
	// branch below), so if the handler discards Warn it also discards Error's
	// less-severe sibling only when Warn>Error in ordering — which it is NOT:
	// slog levels order Error(8) > Warn(4). The minimum level we ever emit is
	// Warn, so when Warn is disabled the Error branch can still fire; we
	// therefore bail only when BOTH are disabled. In the common production
	// case (handler at Info/Warn) this is a no-op gate; under a handler raised
	// above Error it skips the per-line formatting churn entirely.
	if !slog.Default().Enabled(context.Background(), slog.LevelWarn) &&
		!slog.Default().Enabled(context.Background(), slog.LevelError) {
		return
	}
	// R250-CR-15 (#1148): skip fmt.Sprintf when there are no args. Saves
	// an alloc per emitted line and avoids passing untrusted format
	// verbs through the formatter (robfig/cron's PrintfLogger.Error and
	// Info both call Printf with the message as the first arg, which
	// can contain user-controlled content like cron spec strings).
	var msg string
	if len(args) == 0 {
		msg = format
	} else {
		msg = fmt.Sprintf(format, args...)
	}
	msg = strings.TrimRight(msg, "\n")
	// R20260607-SEC-3: sanitize before writing to slog to prevent bidi/C1
	// injection via attacker-influenced content (e.g. cron spec strings).
	// cronPanicMarker is pure ASCII so sanitization does not affect marker matching.
	msg = osutil.SanitizeForLog(msg, 512)
	if strings.Contains(msg, cronPanicMarker) {
		slog.Error("cron logger", "msg", msg)
		return
	}
	slog.Warn("cron logger", "msg", msg)
}
