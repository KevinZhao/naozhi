package server

import (
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/session"
	"golang.org/x/time/rate"
)

// HealthHandler serves the /health endpoint with system status information.
type HealthHandler struct {
	router        *session.Router
	auth          *AuthHandlers
	startedAt     time.Time
	workspaceID   string
	workspaceName string
	// version is the build tag surfaced on /health so external probes can
	// confirm which binary is running without needing dashboard auth. Empty
	// means "unknown" — the field is omitted from the response to preserve
	// the legacy wire shape.
	version         string
	noOutputTimeout time.Duration
	totalTimeout    time.Duration
	// noOutputTimeoutStr / totalTimeoutStr cache the pre-formatted
	// duration strings used in the watchdog sub-object. time.Duration.String()
	// is not trivial (it allocates a short string on every call), and these
	// timeouts never change after router construction, so we format them
	// once here. R59-PERF-H2.
	noOutputTimeoutStr string
	totalTimeoutStr    string
	watchdogNoOut      *atomic.Int64
	watchdogTotal      *atomic.Int64
	nodeAccess         NodeAccessor
	platforms          map[string]struct{} // platform names (read-only after init)
	hubDropped         func() int64        // hub.DroppedMessages
	// dispatcherMetrics returns (message_count, reply_error_count, send_fail_count, last_reply_success).
	// Injected after Start() wires the Dispatcher; nil-safe. last_reply_success
	// is zero-valued until the first successful user-visible reply.
	dispatcherMetrics func() (int64, int64, int64, time.Time)
	// unauthLimiter throttles unauthenticated /health probes per source IP so
	// scanners cannot poll high-frequency to fingerprint uptime / restart
	// timing. Authenticated probes (with a valid Bearer/cookie) bypass this
	// bucket — Prometheus and the dashboard status bar are expected to send
	// credentials. Nil means "limiter not wired" (older test fixtures); in
	// that case no throttle is applied. R226-SEC-7.
	unauthLimiter *ipLimiter
}

// healthUnauthRate is the per-IP sustained rate for anonymous /health
// requests; healthUnauthBurst is the leaky-bucket burst. 60 req/min with
// burst 10 matches the TODO R226-SEC-7 prescription — generous for a load
// balancer's liveness probe (typically 1/s), tight enough to make
// fingerprinting via uptime drift uneconomical for a scanner.
const (
	healthUnauthRate  = rate.Limit(1) // 60 req/min sustained
	healthUnauthBurst = 10
)

// newHealthUnauthLimiter constructs the per-IP limiter used to throttle
// unauthenticated /health requests. Extracted so server.go's wiring stays
// declarative and tests can swap in a tighter bucket. R226-SEC-7.
func newHealthUnauthLimiter(trustedProxy bool) *ipLimiter {
	return newIPLimiterWithProxy(healthUnauthRate, healthUnauthBurst, trustedProxy)
}

// healthWatchdogStats is the /health "watchdog" sub-object. Stack-allocated
// per response so the dashboard-status-bar polling at 1 Hz doesn't pay a
// per-request map[string]any alloc for what is a fixed-shape value. Mirrors
// the R58-PERF-F2 treatment of /api/sessions's watchdog sub-object.
type healthWatchdogStats struct {
	NoOutputKills   int64  `json:"no_output_kills"`
	TotalKills      int64  `json:"total_kills"`
	NoOutputTimeout string `json:"no_output_timeout"`
	TotalTimeout    string `json:"total_timeout"`
}

// healthSessionStats and healthDispatchStats mirror the watchdog treatment —
// named structs with omitempty so /health does not allocate three
// map[string]any objects on every authenticated dashboard poll. R62-PERF-2.
type healthSessionStats struct {
	Active int `json:"active"`
	Total  int `json:"total"`
}

type healthDispatchStats struct {
	MessageCount        int64  `json:"message_count"`
	ReplyErrorCount     int64  `json:"reply_error_count"`
	SendFailCount       int64  `json:"send_fail_count"`
	LastReplySuccessAt  string `json:"last_reply_success_at,omitempty"`
	LastReplySuccessAgo string `json:"last_reply_success_ago,omitempty"`
}

// healthAuthSection is the authenticated-only subset of /health. Held as a
// pointer inside healthResp so unauthenticated probes marshal to just
// {"status":"ok","uptime":"..."}. When non-nil, Go's json package promotes
// the embedded fields into the top-level object so the wire shape stays
// identical to the prior `map[string]any` version. R60-PERF-001.
type healthAuthSection struct {
	// Version is the build tag (git describe injected via -X main.version=...).
	// R229-SEC-7: previously exposed at the top level for unauthenticated
	// probes; moved into the auth-only section so a public /health cannot
	// fingerprint the running binary.
	Version           string                  `json:"version,omitempty"`
	Sessions          healthSessionStats      `json:"sessions"`
	WorkspaceID       string                  `json:"workspace_id"`
	WorkspaceName     string                  `json:"workspace_name"`
	System            map[string]any          `json:"system"`
	Goroutines        int                     `json:"goroutines"`
	Watchdog          healthWatchdogStats     `json:"watchdog"`
	WSDropped         *int64                  `json:"ws_dropped,omitempty"`
	Dispatch          *healthDispatchStats    `json:"dispatch,omitempty"`
	CLIAvailable      bool                    `json:"cli_available"`
	Nodes             map[string]string       `json:"nodes,omitempty"`
	Platforms         map[string]string       `json:"platforms"`
	EventLog          *healthEventLogStats    `json:"eventlog,omitempty"`
	AttachmentTracker *healthAttachTrackStats `json:"attachment_tracker,omitempty"`
}

// healthEventLogStats mirrors session.EventLogHealth over the wire.
// Kept as a server-internal struct so the JSON shape isn't coupled
// to session package refactors — a field rename there won't silently
// break dashboards reading /health.
//
// The `writer_alive` definition per RFC §6.3:
//
//	last_drain_ms_ago < 5000  AND  channel_depth < 0.8 * channel_cap
//
// Both component fields are exposed independently so operators can
// distinguish "writer goroutine deadlocked" from "writer goroutine
// keeping up but channel about to overflow" without parsing the bool.
type healthEventLogStats struct {
	Dir            string `json:"dir"`
	WriterAlive    bool   `json:"writer_alive"`
	ChannelDepth   int    `json:"channel_depth"`
	ChannelCap     int    `json:"channel_cap"`
	LastDrainMsAgo int64  `json:"last_drain_ms_ago"`
	Written        int64  `json:"written_total"`
	Dropped        int64  `json:"dropped_total"`
	Fsyncs         int64  `json:"fsync_total"`
	Malformed      int64  `json:"malformed_total"`
	ReplayLeak     int64  `json:"replay_leak_total"`
	FSType         string `json:"fs_type"`
	FSSupported    bool   `json:"fs_supported"`
}

// healthAttachTrackStats mirrors session.AttachmentTrackerHealth
// over the wire. Kept server-internal for the same reasons as
// healthEventLogStats — the wire shape should not drift when the
// session-level struct evolves.
//
// writer_alive uses the same formula as the event-log tracker:
//
//	last_drain_ms < 5000 AND channel_depth < 0.8 * channel_cap
//
// Per-component fields are exposed so operators can distinguish
// "tracker deadlocked" from "just backed up" without reverse-
// engineering the bool.
type healthAttachTrackStats struct {
	WriterAlive  bool  `json:"writer_alive"`
	ChannelDepth int   `json:"channel_depth"`
	ChannelCap   int   `json:"channel_cap"`
	LastDrainMs  int64 `json:"last_drain_ms"`
	Pending      int   `json:"pending"`
	Written      int64 `json:"written_total"`
	Cleared      int64 `json:"cleared_total"`
	Dropped      int64 `json:"dropped_total"`
	Errors       int64 `json:"meta_error_total"`
}

// healthResp is the JSON response for /health. Prior code built a
// map[string]any per probe (14 interface{} box ops on the hot 1 Hz polling
// path); this named struct is stack-allocated with a lazy pointer for the
// authenticated sub-section. Marshals byte-identically to the old shape.
// R60-PERF-001 / R60-PERF-008.
type healthResp struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
	// Anonymous pointer embed: json package promotes non-nil pointer's
	// fields into the enclosing object, so authenticated probes get the
	// exact same top-level keys as before while unauthenticated probes
	// serialize down to just status/uptime.
	//
	// R229-SEC-7: Version moved into healthAuthSection so unauthenticated
	// probes cannot fingerprint the build.
	*healthAuthSection
}

func (h *HealthHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResp{
		Status: "ok",
		Uptime: time.Since(h.startedAt).Round(time.Second).String(),
	}
	if !h.auth.isAuthenticated(r) {
		// R226-SEC-7: rate-limit anonymous probes per source IP. Authenticated
		// callers (Prometheus, dashboard status bar) bypass the limiter via
		// the early return below — they identify themselves and are trusted.
		if h.unauthLimiter != nil && !h.unauthLimiter.AllowRequest(r) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		writeJSON(w, resp)
		return
	}

	active, total := h.router.Stats()
	auth := &healthAuthSection{
		Version:       h.version,
		Sessions:      healthSessionStats{Active: active, Total: total},
		WorkspaceID:   h.workspaceID,
		WorkspaceName: h.workspaceName,
		System:        systemInfo(),
		Goroutines:    runtime.NumGoroutine(),
		Watchdog: healthWatchdogStats{
			NoOutputKills:   h.watchdogNoOut.Load(),
			TotalKills:      h.watchdogTotal.Load(),
			NoOutputTimeout: h.noOutputTimeoutStr,
			TotalTimeout:    h.totalTimeoutStr,
		},
		CLIAvailable: cliAvailable(h.router.CLIPath()),
	}
	if h.hubDropped != nil {
		n := h.hubDropped()
		auth.WSDropped = &n
	}
	if h.dispatcherMetrics != nil {
		msgs, replyErrs, sendFails, lastReply := h.dispatcherMetrics()
		d := &healthDispatchStats{
			MessageCount:    msgs,
			ReplyErrorCount: replyErrs,
			SendFailCount:   sendFails,
		}
		if !lastReply.IsZero() {
			d.LastReplySuccessAt = lastReply.UTC().Format(time.RFC3339)
			d.LastReplySuccessAgo = time.Since(lastReply).Round(time.Second).String()
		}
		auth.Dispatch = d
	}
	if kn := h.nodeAccess.KnownNodes(); len(kn) > 0 {
		nodeStatus := make(map[string]string, len(kn))
		for id := range kn {
			if nc, ok := h.nodeAccess.GetNode(id); ok {
				nodeStatus[id] = nc.Status()
			} else {
				nodeStatus[id] = "disconnected"
			}
		}
		auth.Nodes = nodeStatus
	}
	platStatus := make(map[string]string, len(h.platforms))
	for name := range h.platforms {
		platStatus[name] = "registered"
	}
	auth.Platforms = platStatus

	if el := h.router.EventLogStats(); el.Enabled {
		auth.EventLog = &healthEventLogStats{
			Dir:            el.Dir,
			WriterAlive:    el.WriterAlive,
			ChannelDepth:   el.ChannelDepth,
			ChannelCap:     el.ChannelCap,
			LastDrainMsAgo: el.LastDrainMsAgo,
			Written:        el.Written,
			Dropped:        el.Dropped,
			Fsyncs:         el.Fsyncs,
			Malformed:      el.Malformed,
			ReplayLeak:     el.ReplayLeak,
			FSType:         el.FSType,
			FSSupported:    el.FSSupported,
		}
	}

	if at := h.router.AttachmentTrackerStats(); at.Enabled {
		auth.AttachmentTracker = &healthAttachTrackStats{
			WriterAlive:  at.WriterAlive,
			ChannelDepth: at.ChannelDepth,
			ChannelCap:   at.ChannelCap,
			LastDrainMs:  at.LastDrainMs,
			Pending:      at.Pending,
			Written:      at.Written,
			Cleared:      at.Cleared,
			Dropped:      at.Dropped,
			Errors:       at.Errors,
		}
	}

	resp.healthAuthSection = auth
	writeJSON(w, resp)
}

// cliAvailable reports whether the CLI binary at path is stat-able. Extracted
// so handleHealth reads linearly without branching on `err != nil` for a single
// boolean — cleaner when the rest of the handler is struct initialization.
func cliAvailable(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// systemInfo returns compact system fingerprint for the workspace info bar.
// Cached after first call since values are static for the process lifetime.
//
// CONTRACT: the returned map is a process-wide singleton — callers MUST
// treat it as read-only. initStaticStats() deep-copies the map before
// handing it to the /api/sessions response path so a future mutable field
// cannot turn the shallow-copy into a cross-goroutine data race; /health
// serialises its own copy via json.Marshal without mutation. Do not mutate
// the returned map from any caller.
var (
	sysInfoOnce sync.Once
	sysInfoVal  map[string]any
)

func systemInfo() map[string]any {
	sysInfoOnce.Do(func() {
		memMB := 0
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if kb, err := strconv.Atoi(fields[1]); err == nil {
							memMB = kb / 1024
						}
					}
					break
				}
			}
		}
		sysInfoVal = map[string]any{
			"os":        runtime.GOOS,
			"arch":      runtime.GOARCH,
			"cpus":      runtime.NumCPU(),
			"memory_mb": memMB,
			"ip_count":  localIPCount(),
		}
	})
	return sysInfoVal
}

// localIPCount returns how many IPv4 addresses are bound to physical/primary
// network interfaces, skipping loopback, docker bridges, and veth pairs.
// The count is exposed to authenticated dashboard users as a liveness signal
// without revealing concrete LAN addresses that could aid internal reconnaissance.
func localIPCount() int {
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0
	}
	count := 0
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		name := iface.Name
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "virbr") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			count++
		}
	}
	return count
}
