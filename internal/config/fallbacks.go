package config

import (
	"sort"
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

// resolveFallbackValues parses, once, the values whose consumers fall back
// instead of failing: the cron time zone, the shim idle timeout and the
// sysession daemon durations. Load calls it; each unusable value is reported
// and cached as zero, which the accessors turn into the default.
func resolveFallbackValues(cfg *Config) {
	if name := strings.TrimSpace(cfg.Cron.Timezone); name != "" && !strings.EqualFold(name, "Local") {
		if _, err := time.LoadLocation(name); err != nil {
			reportFallback("cron.timezone", "fallback", "unknown time zone; cron schedules evaluate in Local")
		}
	}

	cfg.cachedShimIdleTimeout, _ = fallbackDuration(cfg.Session.Shim.IdleTimeout, "session.shim.idle_timeout", false,
		"a shim exits after 4h without a client")

	sys := &cfg.cachedSysession
	names := make([]string, 0, len(cfg.Sysession.Daemons))
	for name := range cfg.Sysession.Daemons {
		names = append(names, name)
	}
	sort.Strings(names)
	sys.daemons = make(map[string]SysessionDaemonDurations, len(names))
	for _, name := range names {
		d, key := cfg.Sysession.Daemons[name], "sysession.daemons."+name+"."
		var out SysessionDaemonDurations
		out.Tick, _ = fallbackDuration(d.Tick, key+"tick", false, "the daemon ticks every 30s")
		out.MinRenameInterval, _ = fallbackDuration(d.MinRenameInterval, key+"min_rename_interval", false,
			"the daemon's default applies")
		out.UploadTTL, _ = fallbackDuration(d.UploadTTL, key+"upload_ttl", true, "the daemon's default applies")
		out.RefTTL, _ = fallbackDuration(d.RefTTL, key+"ref_ttl", true, "the daemon's default applies")
		sys.daemons[name] = out
	}
}

// fallbackDuration parses s for a consumer that has a default. Empty yields
// (0, false). An unparsable or negative value, or zero unless zeroOK, is
// reported under key with the consequence in uses and also yields (0, false);
// set is true only for a usable configured value.
func fallbackDuration(s, key string, zeroOK bool, uses string) (d time.Duration, set bool) {
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err == nil && (d > 0 || (zeroOK && d == 0)) {
		return d, true
	}
	want := "a positive duration"
	if zeroOK {
		want = "a duration of 0 or more"
	}
	reportFallback(key, "fallback", "not "+want+"; "+uses)
	return 0, false
}
