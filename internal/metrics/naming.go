package metrics

import (
	"fmt"
	"regexp"
	"strings"
)

// naming.go codifies the metric naming convention (#622) without renaming
// existing metrics — /debug/vars JSON and the docs/ops/pprof.md doc-sync
// contract pin current names. Name is the factory and ValidName the
// validator for the shape:
//
//	naozhi_<subsystem>_<name>_<suffix>

// NamePrefix is the mandatory leading token; expvar is one global namespace
// shared with the stdlib (cmdline / memstats).
const NamePrefix = "naozhi"

// Subsystem is the second token — the area of the process the metric covers.
// The set is closed: a new subsystem MUST be added here.
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
	SubsystemSysession  Subsystem = "sysession"
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
	SubsystemAttachment, SubsystemCron, SubsystemSysession, SubsystemStartup,
	SubsystemAutoChain, SubsystemProtocol, SubsystemACP, SubsystemMetrics,
}

// Kind selects the metric's semantic and the suffix the name must carry.
// expvar is untyped, so the suffix is the only counter-vs-gauge signal a
// dashboard / Prometheus adapter has.
type Kind int

const (
	// KindCounter is a monotonically-increasing event count. Suffix: _total.
	KindCounter Kind = iota
	// KindGaugeInflight is an instantaneous count of in-flight work. Suffix: _inflight.
	KindGaugeInflight
	// KindGaugeActive is an instantaneous count of active resources. Suffix: _active.
	KindGaugeActive
	// KindGaugeMillis is an instantaneous millisecond duration. Suffix: _ms.
	KindGaugeMillis
	// KindHistogramSum is the running-sum component of a histogram. Suffix: _sum.
	KindHistogramSum
	// KindHistogramBucket is the cumulative-bucket component of a histogram. Suffix: _bucket.
	KindHistogramBucket
)

// validSuffixes is the closed set of trailing tokens; ValidName also accepts
// the _by_backend label-double-write modifier after any of them.
var validSuffixes = []string{"total", "inflight", "active", "ms", "sum", "bucket"}

// labelModifier marks the labeled double-write twin of a metric
// (e.g. naozhi_cli_spawn_total_by_backend).
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

// segmentRE matches one name segment: lowercase snake_case, no
// leading/trailing underscore.
var segmentRE = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)

// Name builds "naozhi_<subsystem>_<name>_<suffix>" or returns the first
// violation. name is the free-form middle (lowercase snake_case) and must
// NOT already carry the kind suffix.
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

// ValidName reports whether full is convention-compliant: naozhi prefix,
// known subsystem, recognised kind suffix.
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
	// Strip the optional label modifier so "..._total_by_backend" validates like "..._total".
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

// matchSubsystem finds the known subsystem prefixing rest; longest match
// wins ("auto_chain" over a hypothetical "auto").
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
