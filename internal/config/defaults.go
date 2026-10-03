package config

import "time"

// Single source of truth for config defaults: each duration is declared once
// and applyDefaults derives the string form via .String(), so applyDefaults
// and parseDurations cannot drift (#630).

// CurrentSchemaVersion is the config schema this binary understands; an older
// (or absent) one is migrated in memory at load, higher is rejected. Bump on
// incompatible YAML shape changes — and add the matching entry to migrations
// (migrations.go), which both Load and `naozhi config migrate` run.
//
// v2 (#2710): the four deprecated aliases — nodes, session.workspace,
// session.auto_chain and --append-system-prompt inside agents[].args — leave
// the file.
const CurrentSchemaVersion = 2

const (
	defaultServerAddr    = ":8080"
	defaultLogLevel      = "info"
	defaultSessionCWD    = "~/.naozhi/workspace"
	defaultQueueMode     = "collect"
	defaultQueueMaxDepth = 20
)

// Duration defaults shared by applyDefaults (.String()) and parseDurations.
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

// Defaults for the values resolveFallbackValues caches; the accessors apply
// them to a zero (unset or unusable) cached value.
const (
	defaultShimIdleTimeout      = 4 * time.Hour
	defaultShimWatchdogTimeout  = 30 * time.Minute
	defaultShimMaxBufferBytes   = 50 << 20
	defaultSysessionTick        = 30 * time.Second
	defaultSysessionJSONLMaxAge = 7 * 24 * time.Hour
)

// orDefault returns v, or def when v is zero.
func orDefault[T time.Duration | int64](v, def T) T {
	if v == 0 {
		return def
	}
	return v
}
