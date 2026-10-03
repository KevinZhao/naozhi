package config

import (
	"math"
	"sort"
	"strconv"
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
// instead of failing: the cron time zone, the shim lifetimes and buffer size,
// the stdio cap, and the sysession durations. Load calls it; each unusable value is reported
// and cached as zero, which the accessors turn into the default.
func resolveFallbackValues(cfg *Config) {
	if name := strings.TrimSpace(cfg.Cron.Timezone); name != "" && !strings.EqualFold(name, "Local") {
		if _, err := time.LoadLocation(name); err != nil {
			reportFallback("cron.timezone", "fallback", "unknown time zone; cron schedules evaluate in Local")
		}
	}

	shim := cfg.Session.Shim
	cfg.cachedShimIdleTimeout, _ = fallbackDuration(shim.IdleTimeout, "session.shim.idle_timeout", false,
		"a shim exits after 4h without a client")
	cfg.cachedShimWatchdogTimeout, _ = fallbackDuration(shim.WatchdogTimeout, "session.shim.disconnect_watchdog", false,
		"the disconnect watchdog fires after 30m")
	cfg.cachedShimMaxBufferBytes, _ = fallbackSize(shim.MaxBufferBytes, "session.shim.max_buffer_bytes", false,
		"the ring buffer holds 50MB")
	stdioMax, set := fallbackSize(cfg.Log.StdioMaxSize, "log.stdio_max_size", true,
		"stdout and stderr are capped at 64MB")
	cfg.cachedStdioMaxSize, cfg.stdioCapOff = stdioMax, set && stdioMax == 0

	sys := &cfg.cachedSysession
	sys.tickTimeout, _ = fallbackDuration(cfg.Sysession.TickTimeout, "sysession.tick_timeout", false,
		"a tick is capped at 30s")
	maxAge, set := fallbackDuration(cfg.Sysession.Runner.JSONLMaxAge, "sysession.runner.jsonl_max_age", true,
		"sys-session logs are kept for 7 days")
	sys.jsonlMaxAge, sys.jsonlSweepOff = maxAge, set && maxAge == 0

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

// fallbackSize is fallbackDuration for a byte size such as "50MB".
func fallbackSize(s, key string, zeroOK bool, uses string) (n int64, set bool) {
	if s == "" {
		return 0, false
	}
	n, ok := parseByteSize(s)
	if ok && (n > 0 || (zeroOK && n == 0)) {
		return n, true
	}
	want := "a positive size such as 50MB"
	if zeroOK {
		want = "a size such as 64MB, or 0"
	}
	reportFallback(key, "fallback", "not "+want+"; "+uses)
	return 0, false
}

// parseByteSize parses a size such as "50MB", "1gb", "512KB" or "4096B" (case
// insensitive, binary multiples; a bare number is bytes). ok is false for any
// other form and for a value that overflows int64.
func parseByteSize(s string) (n int64, ok bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	multiplier := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			s, multiplier = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n > math.MaxInt64/multiplier || n < math.MinInt64/multiplier {
		return 0, false
	}
	return n * multiplier, true
}
