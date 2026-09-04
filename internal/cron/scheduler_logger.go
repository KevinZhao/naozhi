package cron

// scheduler_logger.go: the robfig/cron Printf-logger adapter routed through slog.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// slogPrintfLogger satisfies the Printf interface robfig/cron's PrintfLogger
// expects, routing every emitted line through slog.
//
// robfig/cron wraps this via the non-verbose PrintfLogger, which compiles
// Info() out entirely (SkipIfStillRunning's Info never reaches Printf); only
// Error() lines do — recover-panic recoveries and schedule parse failures.
// Panic recoveries log at Error; anything else stays at Warn so upstream
// changes that route new events through Error remain visible.
type slogPrintfLogger struct{}

// cronPanicMarker is the substring in robfig/cron-emitted log lines that
// escalates to slog.Error rather than slog.Warn. robfig/cron's Recover wrapper
// calls logger.Error(err, "panic", "stack", ...) and the printfLogger Error
// formatter renders the msg verbatim, so "panic" appears in every
// recover-emitted line and in no other Error path of the library.
const cronPanicMarker = "panic"

func (slogPrintfLogger) Printf(format string, args ...any) {
	// Every line lands at Warn or Error, so bail only when BOTH are disabled
	// and skip the Sprintf / TrimRight / Contains churn (#931).
	if !slog.Default().Enabled(context.Background(), slog.LevelWarn) &&
		!slog.Default().Enabled(context.Background(), slog.LevelError) {
		return
	}
	// No-args path skips fmt.Sprintf: saves an alloc and keeps untrusted
	// format verbs out of the formatter (robfig/cron passes the message as
	// the format arg, which can carry cron spec strings) (#1148).
	var msg string
	if len(args) == 0 {
		msg = format
	} else {
		msg = fmt.Sprintf(format, args...)
	}
	msg = strings.TrimRight(msg, "\n")
	// Sanitize so attacker-influenced content (e.g. cron spec strings) cannot
	// inject bidi/C1; cronPanicMarker is pure ASCII so matching is unaffected.
	msg = osutil.SanitizeForLog(msg, 512)
	if strings.Contains(msg, cronPanicMarker) {
		slog.Error("cron logger", "msg", msg)
		return
	}
	slog.Warn("cron logger", "msg", msg)
}
