package metrics

import (
	"fmt"
	"regexp"
	"strings"
)

// naming.go codifies the metric naming convention so the "8 prefixes, no
// enforced suffix" chaos (R247-ARCH-6 / #622) stops growing. It does NOT
// rename any existing metric — the on-disk /debug/vars JSON shape and the
// docs/ops/pprof.md doc-sync contract both pin the current names — it gives
// new code a single factory (Name) plus a validator (ValidName) so every
// future metric is forced into the shape:
//
//	naozhi_<subsystem>_<name>_<suffix>
//
// where <subsystem> is one of the registered Subsystem* constants and
// <suffix> matches the Kind. Today this is the convention layer the proposed
// metrics.New(subsystem, name, kind) factory needs; the expvar→Prometheus
// collector adapter is a follow-up that can build on Name() unchanged.

// NamePrefix is the mandatory leading token on every naozhi metric. expvar
// is a single global namespace shared with the stdlib (cmdline / memstats),
// so the prefix is what keeps naozhi metrics greppable and collision-free.
const NamePrefix = "naozhi"

// Subsystem is the second token — the area of the process the metric covers.
// The set is closed: a new subsystem MUST be added here so the registry of
// known prefixes stays discoverable in one place instead of being implied by
// scattered string literals.
type Subsystem string

const (
	SubsystemSession    Subsystem = "session"
	SubsystemCLI        Subsystem = "cli"
	SubsystemWS         Subsystem = "ws"
	SubsystemShim       Subsystem = "shim"
	SubsystemSpawn      Subsystem = "spawn"
	SubsystemPanic      Subsystem = "panic"
	SubsystemInterrupt  Subsystem = "interrupt"
	SubsystemEventlog   Subsystem = "eventlog"
	SubsystemAttachment Subsystem = "attachment"
	SubsystemCron       Subsystem = "cron"
	SubsystemStartup    Subsystem = "startup"
	SubsystemAutoChain  Subsystem = "auto_chain"
	SubsystemProtocol   Subsystem = "protocol"
	SubsystemACP        Subsystem = "acp"
	SubsystemMetrics    Subsystem = "metrics"
)

// KnownSubsystems is the canonical list, used by tests to assert every
// registered metric falls under a declared subsystem.
var KnownSubsystems = []Subsystem{
	SubsystemSession, SubsystemCLI, SubsystemWS, SubsystemShim,
	SubsystemSpawn, SubsystemPanic, SubsystemInterrupt, SubsystemEventlog,
	SubsystemAttachment, SubsystemCron, SubsystemStartup, SubsystemAutoChain,
	SubsystemProtocol, SubsystemACP, SubsystemMetrics,
}

// Kind selects the metric's semantic and the suffix the name must carry.
// expvar is untyped (every value is an int64), so the suffix is the only
// signal a dashboard / Prometheus adapter has to tell a monotonic counter
// from an instantaneous gauge.
type Kind int

const (
	// KindCounter is a monotonically-increasing event count. Suffix: _total.
	KindCounter Kind = iota
	// KindGaugeInflight is an instantaneous count of in-flight work.
	// Suffix: _inflight.
	KindGaugeInflight
	// KindGaugeActive is an instantaneous count of active resources
	// (e.g. live sessions). Suffix: _active.
	KindGaugeActive
	// KindGaugeMillis is an instantaneous millisecond duration (startup
	// phase timings). Suffix: _ms.
	KindGaugeMillis
	// KindHistogramSum is the running-sum component of a histogram.
	// Suffix: _sum.
	KindHistogramSum
	// KindHistogramBucket is the cumulative-bucket component of a
	// histogram. Suffix: _bucket.
	KindHistogramBucket
)

// validSuffixes is the closed set of recognised trailing tokens. ValidName
// accepts a name ending in any of these, optionally followed by the
// _by_backend label-double-write modifier (Multi-Backend RFC §10).
var validSuffixes = []string{"total", "inflight", "active", "ms", "sum", "bucket"}

// labelModifier marks the legacy/labeled double-write twin of a metric
// (e.g. naozhi_cli_spawn_total_by_backend alongside naozhi_cli_spawn_total).
const labelModifier = "by_backend"

// suffix returns the mandatory trailing token for the kind.
func (k Kind) suffix() string {
	switch k {
	case KindCounter:
		return "total"
	case KindGaugeInflight:
		return "inflight"
	case KindGaugeActive:
		return "active"
	case KindGaugeMillis:
		return "ms"
	case KindHistogramSum:
		return "sum"
	case KindHistogramBucket:
		return "bucket"
	default:
		return ""
	}
}

// segmentRE matches a single valid name segment: lowercase, digits, no
// leading/trailing underscore, internal underscores allowed.
var segmentRE = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)

// Name builds a convention-compliant metric name from its parts, or returns
// an error describing the first violation. The returned name is
// "naozhi_<subsystem>_<name>_<suffix>".
//
// name is the free-form middle portion (e.g. "create", "run_failed",
// "auth_fail_invalid_token"); it must be lowercase snake_case and must NOT
// already carry the kind suffix (Name appends it).
func Name(sub Subsystem, name string, kind Kind) (string, error) {
	if !isKnownSubsystem(sub) {
		return "", fmt.Errorf("metrics.Name: unknown subsystem %q (add it to KnownSubsystems)", sub)
	}
	if !segmentRE.MatchString(name) {
		return "", fmt.Errorf("metrics.Name: name %q must be lowercase snake_case with no leading/trailing underscore", name)
	}
	suf := kind.suffix()
	if suf == "" {
		return "", fmt.Errorf("metrics.Name: unknown kind %d", int(kind))
	}
	if strings.HasSuffix(name, "_"+suf) || name == suf {
		return "", fmt.Errorf("metrics.Name: name %q must not include the %q suffix; Name appends it", name, suf)
	}
	return fmt.Sprintf("%s_%s_%s_%s", NamePrefix, sub, name, suf), nil
}

// ValidName reports whether full is a convention-compliant metric name:
// prefix == naozhi, second token is a known subsystem, and the name ends in
// a recognised kind suffix. Used by the conformance test and available to
// any future registration helper that wants to fail loud on a typo.
func ValidName(full string) bool {
	if !strings.HasPrefix(full, NamePrefix+"_") {
		return false
	}
	rest := strings.TrimPrefix(full, NamePrefix+"_")
	sub, ok := matchSubsystem(rest)
	if !ok {
		return false
	}
	tail := strings.TrimPrefix(rest, string(sub)+"_")
	if tail == "" {
		return false
	}
	// Strip the optional labeled double-write modifier so the base suffix
	// check below treats e.g. "..._total_by_backend" the same as "..._total".
	tail = strings.TrimSuffix(tail, "_"+labelModifier)
	if tail == "" {
		return false
	}
	for _, suf := range validSuffixes {
		if tail == suf || strings.HasSuffix(tail, "_"+suf) {
			return true
		}
	}
	return false
}

func isKnownSubsystem(sub Subsystem) bool {
	for _, s := range KnownSubsystems {
		if s == sub {
			return true
		}
	}
	return false
}

// matchSubsystem finds the known subsystem that prefixes rest. Longest match
// wins so "auto_chain" is preferred over a hypothetical "auto". Returns the
// matched subsystem and whether one was found.
func matchSubsystem(rest string) (Subsystem, bool) {
	var best Subsystem
	found := false
	for _, s := range KnownSubsystems {
		p := string(s) + "_"
		if strings.HasPrefix(rest, p) && len(string(s)) > len(string(best)) {
			best = s
			found = true
		}
	}
	return best, found
}
