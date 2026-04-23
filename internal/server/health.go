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
	watchdogNoOut   *atomic.Int64
	watchdogTotal   *atomic.Int64
	nodeAccess      NodeAccessor
	platforms       map[string]struct{} // platform names (read-only after init)
	hubDropped      func() int64        // hub.DroppedMessages
	// dispatcherMetrics returns (message_count, reply_error_count, send_fail_count, last_reply_success).
	// Injected after Start() wires the Dispatcher; nil-safe. last_reply_success
	// is zero-valued until the first successful user-visible reply.
	dispatcherMetrics func() (int64, int64, int64, time.Time)
}

func (h *HealthHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"status": "ok",
		"uptime": time.Since(h.startedAt).Round(time.Second).String(),
	}
	// Extended system info only for authenticated requests
	if h.auth.isAuthenticated(r) {
		active, total := h.router.Stats()
		resp["sessions"] = map[string]int{"active": active, "total": total}
		resp["workspace_id"] = h.workspaceID
		resp["workspace_name"] = h.workspaceName
		resp["system"] = systemInfo()
		resp["goroutines"] = runtime.NumGoroutine()
		resp["watchdog"] = map[string]any{
			"no_output_kills":   h.watchdogNoOut.Load(),
			"total_kills":       h.watchdogTotal.Load(),
			"no_output_timeout": h.noOutputTimeout.String(),
			"total_timeout":     h.totalTimeout.String(),
		}
		if h.hubDropped != nil {
			resp["ws_dropped"] = h.hubDropped()
		}
		if h.dispatcherMetrics != nil {
			msgs, replyErrs, sendFails, lastReply := h.dispatcherMetrics()
			dispatch := map[string]any{
				"message_count":     msgs,
				"reply_error_count": replyErrs,
				"send_fail_count":   sendFails,
			}
			if !lastReply.IsZero() {
				dispatch["last_reply_success_at"] = lastReply.UTC().Format(time.RFC3339)
				dispatch["last_reply_success_ago"] = time.Since(lastReply).Round(time.Second).String()
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
