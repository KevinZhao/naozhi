package config

import (
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/spawndiag"
)

// A value naozhi cannot use is replaced so the process still starts, and the
// replacement is reported as a config diag rather than a bare log line: at
// startup the diag logs a warning, and `naozhi config check` exits 1 on it, so
// a typo cannot pass the check and then run with a default (#2897 C4).

// reportFallback records that key's configured value was replaced.
func reportFallback(key, action, reason string) {
	spawndiag.Emit("config", []spawndiag.Diag{{
		Layer: "config-invalid", Key: key, Action: action, Reason: reason,
	}})
}

// reportUnusableValues checks the values whose consumers fall back instead of
// failing: the cron time zone and the sysession tick durations. Load calls it
// once; the consumers keep their fallbacks.
func reportUnusableValues(cfg *Config) {
	if name := strings.TrimSpace(cfg.Cron.Timezone); name != "" && !strings.EqualFold(name, "Local") {
		if _, err := time.LoadLocation(name); err != nil {
			reportFallback("cron.timezone", "fallback", "unknown time zone; cron schedules evaluate in Local")
		}
	}
	if v := cfg.Sysession.TickTimeout; v != "" {
		if _, err := time.ParseDuration(v); err != nil {
			reportFallback("sysession.tick_timeout", "fallback", "not a duration; a tick is capped at 30s")
		}
	}
	for name, d := range cfg.Sysession.Daemons {
		if d.Tick == "" {
			continue
		}
		if _, err := time.ParseDuration(d.Tick); err != nil {
			reportFallback("sysession.daemons."+name+".tick", "fallback", "not a duration; the daemon ticks every 30s")
		}
	}
}
