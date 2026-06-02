package config

import "time"

// defaults.go is the single source of truth for config default values.
//
// Background (R247-ARCH-8 / #630): defaults and their parse-fallbacks used to
// live in two places — applyDefaults() wrote string literals ("30m", "72h")
// onto empty fields, while parseDurations() independently passed time.Duration
// fallbacks (30*time.Minute, 72*time.Hour) for the same fields. The two had to
// stay numerically equal but nothing enforced it, so a change in one spot could
// silently diverge from the other.
//
// Each duration default below is declared once as a time.Duration; the string
// form consumed by applyDefaults is derived via .String() so the two can never
// drift. New defaults SHOULD be added here rather than inline.
const (
	defaultServerAddr    = ":8080"
	defaultLogLevel      = "info"
	defaultSessionCWD    = "~/.naozhi/workspace"
	defaultQueueMode     = "collect"
	defaultQueueMaxDepth = 20
)

// Duration defaults. The string field on the Config is set from .String() in
// applyDefaults; the same constant is the fallback in parseDurations.
const (
	defaultSessionTTL        = 30 * time.Minute
	defaultSessionPruneTTL   = 72 * time.Hour
	defaultNoOutputTimeout   = 2 * time.Minute
	defaultTotalTimeout      = 5 * time.Minute
	defaultCronExecTimeout   = 5 * time.Minute
	defaultQueueCollectDelay = 500 * time.Millisecond
	defaultCronJitterMax     = 2 * time.Minute
	cronJitterMaxHardCap     = 10 * time.Minute
)
