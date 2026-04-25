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
	router          *session.Router
	auth            *AuthHandlers
	startedAt       time.Time
	workspaceID     string
	workspaceName   string
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

func (h *HealthHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"status": "ok",
		"uptime": time.Since(h.startedAt).Round(time.Second).String(),
	}
	// Extended system info only for authenticated requests
	if h.auth.isAuthenticated(r) {
		active, total := h.router.Stats()
		resp["sessions"] = healthSessionStats{Active: active, Total: total}
		resp["workspace_id"] = h.workspaceID
		resp["workspace_name"] = h.workspaceName
		resp["system"] = systemInfo()
		resp["goroutines"] = runtime.NumGoroutine()
		resp["watchdog"] = healthWatchdogStats{
			NoOutputKills:   h.watchdogNoOut.Load(),
			TotalKills:      h.watchdogTotal.Load(),
			NoOutputTimeout: h.noOutputTimeoutStr,
			TotalTimeout:    h.totalTimeoutStr,
		}
		if h.hubDropped != nil {
			resp["ws_dropped"] = h.hubDropped()
		}
		if h.dispatcherMetrics != nil {
			msgs, replyErrs, sendFails, lastReply := h.dispatcherMetrics()
			dispatch := healthDispatchStats{
				MessageCount:    msgs,
				ReplyErrorCount: replyErrs,
				SendFailCount:   sendFails,
			}
			if !lastReply.IsZero() {
				dispatch.LastReplySuccessAt = lastReply.UTC().Format(time.RFC3339)
				dispatch.LastReplySuccessAgo = time.Since(lastReply).Round(time.Second).String()
			}
			resp["dispatch"] = dispatch
		}

		// Check CLI binary availability
		cliOK := true
		if _, err := os.Stat(h.router.CLIPath()); err != nil {
			cliOK = false
		}
		resp["cli_available"] = cliOK

		// Node connection status
		if kn := h.nodeAccess.KnownNodes(); len(kn) > 0 {
			nodeStatus := make(map[string]string, len(kn))
			for id := range kn {
				if nc, ok := h.nodeAccess.GetNode(id); ok {
					nodeStatus[id] = nc.Status()
				} else {
					nodeStatus[id] = "disconnected"
				}
			}
			resp["nodes"] = nodeStatus
		}

		// Platform status
		platStatus := make(map[string]string, len(h.platforms))
		for name := range h.platforms {
			platStatus[name] = "registered"
		}
		resp["platforms"] = platStatus
	}
	writeJSON(w, resp)
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
