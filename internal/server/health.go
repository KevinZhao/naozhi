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

// handleLivez serves /livez — Kubernetes-style liveness probe. Returns
// 200 if the process is alive (i.e. this goroutine got scheduled and the
// HTTP serve loop is running). NEVER touches dependencies (router, hub,
// CLI, eventlog) so a backed-up dependency cannot cause a liveness failure
// and trigger a restart loop. Always unauthenticated; the response body is
// the static literal "ok\n" so an attacker scanning the endpoint cannot
// fingerprint the deployment beyond "naozhi is up". R247-ARCH-1 (#609).
//
// Restart loop hazard: every K8s probe cycle that observes a non-200 here
// kills the process. Anything that can wedge under load (cron, transcript
// store, eventlog drain) must therefore stay OUT of this handler — those
// concerns belong on /readyz where a temporary not-ready merely removes
// the pod from the load-balancer rotation without restarting it.
func (h *HealthHandler) handleLivez(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz serves /readyz — Kubernetes-style readiness probe. Returns
// 200 only when the process is ready to accept traffic: HTTP listener
// bound (implicit — this handler is registered after Start) and the
// router has finished its startup wiring (hub != nil). When not ready,
// returns 503 with a short reason so operators tailing the probe history
// see why traffic was blackholed. Like /livez, never returns the rich
// stats / version / sub-objects so an unauthenticated probe cannot
// fingerprint the binary beyond "ready"/"not ready". R247-ARCH-1 (#609).
//
// Why this is separate from /livez: a wedged dependency (eventlog drain
// stuck, scheduler restart panic loop) should drop the pod from the LB
// rotation but NOT trigger a kill — readiness expresses "send me traffic"
// while liveness expresses "I'm alive at all". Conflating them turns a
// transient dep wobble into a restart storm. The split mirrors the
// k8s probe contract every orchestrator (k8s, ECS, Nomad, ALB target
// groups via separate health-check paths) understands natively.
func (h *HealthHandler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Router presence is the minimal "wired" check — Start() bolts every
	// handler onto a non-nil router before the listener accepts traffic,
	// so router==nil here means the constructor partially-initialised
	// HealthHandler (test harness) and the probe should fail closed.
	if h.router == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: router unavailable\n"))
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

// handleHealth serves /health with a two-tier response:
//
//   - Unauthenticated probes: status + uptime only. Intentionally cheap so
//     orchestrators (k8s liveness, ALB target-group checks) don't need a
//     token. R246-SEC-11 (#819): the unauth branch is now gated by the
//     same per-IP unauthDashLimiter that throttles the login-page render
//     so a scanner cannot fingerprint deployment uptime at unbounded rate.
//     Authenticated callers skip the gate (the auth check happens first
//     so legitimate dashboard polls never count against the bucket).
//     The limiter is nil-safe; tests that build HealthHandler without
//     an AuthHandlers fall through to the previous unthrottled path.
//
//   - Authenticated probes (operator dashboard, /api/sessions polling):
//     full sub-objects. Already throttled at the HTTP layer by the
//     dashboard's 1 Hz poll cadence; lateral moves from a stolen token
//     would face the same poll budget as a legitimate dashboard tab.
func (h *HealthHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResp{
		Status: "ok",
		Uptime: time.Since(h.startedAt).Round(time.Second).String(),
	}
	if !h.auth.IsAuthenticated(r) {
		// R246-SEC-11 (#819): per-IP cap so an attacker scanning across
		// time cannot enumerate uptime to fingerprint deploy/restart
		// cadence. unauthDashAllow returns true when the limiter is not
		// wired (test harness without server.New) so this stays a no-op
		// for fixtures that bypass the bucket.
		if h.auth != nil && !h.auth.UnauthDashAllow(clientIP(r, h.auth.TrustedProxy)) {
			// JSON envelope (errRespRetry, R247-ARCH-3 / #612 / #451) keeps the
			// /health error path consistent with its writeJSON success path and
			// carries retry_after in the body for fetch wrappers that drop the
			// Retry-After header. errRespRetry also sets the header itself.
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

	// R247-ARCH-12 (#1052): the per-subsystem auth-section fields
	// (ws_dropped, dispatch, eventlog, attachment_tracker) route through
	// the HealthProbe factories defined in health_probe.go instead of
	// inlining each field copy here. The factories are the single source
	// of truth for each subsystem's wire mapping and keep the
	// disabled-as-noop (nil pointer / nil closure → omitempty) contract,
	// so the JSON output stays byte-identical to the prior inline form.
	// Top-level fields that read many HealthHandler-private values at once
	// (sessions / system / nodes / platforms / watchdog) remain inline.
	for _, probe := range h.subsystemProbes() {
		probe(auth)
	}

	resp.healthAuthSection = auth
	writeJSON(w, resp)
}

// cliAvailable / cliAvailableAt / cliAvailEntry / cliAvailCacheTTL /
// cliAvailCache / systemInfo / sysInfoOnce / sysInfoVal / localIPCount
// 抽到 health_systeminfo.go (Phase 5-prep, 2026-05-28).
