package config

import "time"

// Single source of truth for config defaults: each duration is declared once
// and applyDefaults derives the string form via .String(), so applyDefaults
// and parseDurations cannot drift (#630).

// CurrentSchemaVersion is the config schema this binary understands; absent
// (0) is treated as current, higher is rejected at load. Bump on incompatible
// YAML shape changes.
const CurrentSchemaVersion = 1

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
