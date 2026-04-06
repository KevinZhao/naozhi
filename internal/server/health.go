package server

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	active, total := s.router.Stats()
	resp := map[string]interface{}{
		"status":   "ok",
		"uptime":   time.Since(s.startedAt).Round(time.Second).String(),
		"sessions": map[string]int{"active": active, "total": total},
	}
	// Extended system info only for authenticated requests
	if s.isAuthenticated(r) {
		resp["workspace_id"] = s.workspaceID
		resp["workspace_name"] = s.workspaceName
		resp["system"] = systemInfo()
		resp["goroutines"] = runtime.NumGoroutine()
		resp["watchdog"] = map[string]any{
			"no_output_kills":   s.watchdogNoOutputKills.Load(),
			"total_kills":       s.watchdogTotalKills.Load(),
			"no_output_timeout": s.noOutputTimeout.String(),
			"total_timeout":     s.totalTimeout.String(),
		}
		if s.hub != nil {
			resp["ws_dropped"] = s.hub.DroppedMessages()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("encode health response", "err", err)
	}
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
			"ips":       localIPs(),
		}
	})
	return sysInfoVal
}

// localIPs returns IPv4 addresses from physical/primary network interfaces,
// skipping loopback, docker bridges, and veth pairs.
func localIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		// Skip docker/veth/bridge virtual interfaces
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
			ips = append(ips, ipnet.IP.String())
		}
	}
	return ips
}
