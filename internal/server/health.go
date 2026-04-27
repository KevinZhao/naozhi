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
	Sessions      healthSessionStats   `json:"sessions"`
	WorkspaceID   string               `json:"workspace_id"`
	WorkspaceName string               `json:"workspace_name"`
	System        map[string]any       `json:"system"`
	Goroutines    int                  `json:"goroutines"`
	Watchdog      healthWatchdogStats  `json:"watchdog"`
	WSDropped     *int64               `json:"ws_dropped,omitempty"`
	Dispatch      *healthDispatchStats `json:"dispatch,omitempty"`
	CLIAvailable  bool                 `json:"cli_available"`
	Nodes         map[string]string    `json:"nodes,omitempty"`
	Platforms     map[string]string    `json:"platforms"`
}

// healthResp is the JSON response for /health. Prior code built a
// map[string]any per probe (14 interface{} box ops on the hot 1 Hz polling
// path); this named struct is stack-allocated with a lazy pointer for the
// authenticated sub-section. Marshals byte-identically to the old shape.
// R60-PERF-001 / R60-PERF-008.
type healthResp struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
	// Version is the build tag (e.g. git describe output injected via
	// -X main.version=...). Exposed at the top level so unauthenticated
	// probes (load balancers, uptime monitors) can confirm which binary is
	// live without needing the dashboard token. Empty → field omitted so
	// older deployments that never set the ldflag keep the legacy shape.
	Version string `json:"version,omitempty"`
	// Anonymous pointer embed: json package promotes non-nil pointer's
	// fields into the enclosing object, so authenticated probes get the
	// exact same top-level keys as before while unauthenticated probes
	// serialize down to just status/uptime.
	*healthAuthSection
}

func (h *HealthHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResp{
		Status:  "ok",
		Uptime:  time.Since(h.startedAt).Round(time.Second).String(),
		Version: h.version,
	}
	if !h.auth.isAuthenticated(r) {
		writeJSON(w, resp)
		return
	}

	active, total := h.router.Stats()
	auth := &healthAuthSection{
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
