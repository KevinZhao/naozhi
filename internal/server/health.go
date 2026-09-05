package server

import (
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/session"
)

// HealthHandler serves the /health endpoint with system status information.
type HealthHandler struct {
	router        *session.Router
	auth          *auth.Handlers
	startedAt     time.Time
	workspaceID   string
	workspaceName string
	// version is the build tag surfaced on the authenticated part of /health;
	// empty means unknown and the field is omitted.
	version         string
	noOutputTimeout time.Duration
	totalTimeout    time.Duration
	// noOutputTimeoutStr / totalTimeoutStr are the pre-formatted watchdog
	// duration strings (timeouts never change after construction).
	noOutputTimeoutStr string
	totalTimeoutStr    string
	watchdogNoOut      *atomic.Int64
	watchdogTotal      *atomic.Int64
	nodeAccess         NodeAccessor
	platforms          map[string]struct{} // platform names (read-only after init)
	// platformsStatus is the pre-built {name: "registered"} map served as the
	// /health `platforms` sub-object. Read-only after init; never mutated.
	platformsStatus map[string]string
	hubDropped      func() int64 // hub.DroppedMessages
	// dispatcherMetrics returns (message_count, reply_error_count, send_fail_count, last_reply_success).
	// Injected after Start() wires the Dispatcher; nil-safe. last_reply_success
	// is zero-valued until the first successful user-visible reply.
	dispatcherMetrics func() (int64, int64, int64, time.Time)
	// configSHA256 / configLoadedAt / configPath are the loaded config's
	// fingerprint (#2538); auth-only fields, empty when unknown.
	configSHA256   string
	configLoadedAt time.Time
	configPath     string
}

// healthWatchdogStats is the /health "watchdog" sub-object.
type healthWatchdogStats struct {
	NoOutputKills   int64  `json:"no_output_kills"`
	TotalKills      int64  `json:"total_kills"`
	NoOutputTimeout string `json:"no_output_timeout"`
	TotalTimeout    string `json:"total_timeout"`
}

// healthSessionStats and healthDispatchStats are fixed-shape /health sub-objects.
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
// {"status":"ok","uptime":"..."}; when non-nil its fields are promoted to
// the top-level object.
type healthAuthSection struct {
	// Version is the build tag. Auth-only so a public /health cannot
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
	// ConfigSHA256 / ConfigLoadedAt / ConfigPath fingerprint the config the
	// process loaded (#2538). Auth-only by construction (this struct is the
	// authenticated section), so a public probe cannot read the hash or path.
	ConfigSHA256   string `json:"config_sha256,omitempty"`
	ConfigLoadedAt string `json:"config_loaded_at,omitempty"`
	ConfigPath     string `json:"config_path,omitempty"`
	Nodes             map[string]string       `json:"nodes,omitempty"`
	Platforms         map[string]string       `json:"platforms"`
	EventLog          *healthEventLogStats    `json:"eventlog,omitempty"`
	AttachmentTracker *healthAttachTrackStats `json:"attachment_tracker,omitempty"`
}

// healthEventLogStats mirrors session.EventLogHealth over the wire; kept
// server-internal so the JSON shape is decoupled from session refactors.
//
// writer_alive (RFC §6.3):
//
//	last_drain_ms_ago < 5000  AND  channel_depth < 0.8 * channel_cap
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

// healthAttachTrackStats mirrors session.AttachmentTrackerHealth over the
// wire (server-internal, same reasons as healthEventLogStats).
//
// writer_alive:
//
//	last_drain_ms < 5000 AND channel_depth < 0.8 * channel_cap
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

// healthResp is the JSON response for /health.
type healthResp struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
	// Anonymous pointer embed: nil for unauthenticated probes (status/uptime
	// only); non-nil promotes the auth fields to the top level.
	*healthAuthSection
}

// handleLivez serves /livez — Kubernetes-style liveness probe. MUST NOT
// touch dependencies (router, hub, CLI, eventlog): a non-200 here restarts
// the process, so anything that can wedge belongs on /readyz instead.
// Unauthenticated; the static body reveals nothing beyond "up" (#609).
func (h *HealthHandler) handleLivez(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz serves /readyz — Kubernetes-style readiness probe. 503 with a
// short reason when not ready; never returns stats/version so an
// unauthenticated probe cannot fingerprint the binary (#609).
func (h *HealthHandler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// router==nil means a partially-initialised HealthHandler; fail closed.
	if h.router == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: router unavailable\n"))
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

// handleHealth serves /health with a two-tier response:
//
//   - Unauthenticated probes: status + uptime only, gated by the per-IP
//     unauthDashLimiter so a scanner cannot enumerate uptime at unbounded
//     rate (#819). The limiter is nil-safe for harnesses without auth.
//   - Authenticated probes: full sub-objects.
func (h *HealthHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Set at the top so every exit path (incl. the rate-limit branch) carries them.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	resp := healthResp{
		Status: "ok",
		Uptime: time.Since(h.startedAt).Round(time.Second).String(),
	}
	if !h.auth.IsAuthenticated(r) {
		// Fail closed when the client IP is unresolvable in trusted-proxy mode
		// (XFF stripped) instead of sharing the unknownIPKey bucket, so one
		// direct-to-origin attacker cannot starve the budget for every other
		// XFF-less caller (#2120). Mirrors HandleLogin.
		if h.auth != nil &&
			(!requestHasResolvableClientIP(r, h.auth.TrustedProxy) ||
				!h.auth.UnauthDashAllow(clientIP(r, h.auth.TrustedProxy))) {
			errRespRetry(w, http.StatusTooManyRequests, "rate_limited", "too many requests", 60)
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
		ConfigSHA256: h.configSHA256,
		ConfigPath:   h.configPath,
	}
	if !h.configLoadedAt.IsZero() {
		auth.ConfigLoadedAt = h.configLoadedAt.Format(time.RFC3339)
	}
	if nodeStatus := h.nodeAccess.NodesStatus(); len(nodeStatus) > 0 {
		auth.Nodes = nodeStatus
	}
	auth.Platforms = h.platformsStatus

	// Per-subsystem fields (ws_dropped, dispatch, eventlog, attachment_tracker)
	// come from the HealthProbe factories in health_probe.go.
	for _, probe := range h.subsystemProbes() {
		probe(auth)
	}

	resp.healthAuthSection = auth
	writeJSON(w, resp)
}
