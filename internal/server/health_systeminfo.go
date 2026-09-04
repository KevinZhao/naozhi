// /health 用的"读一次缓存一次"系统信息 helpers：CLI 二进制 stat 缓存、
// 进程级 OS/CPU/Mem 指纹、物理网卡 IPv4 计数。
package server

import (
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cliAvailable reports whether the CLI binary at path is stat-able.
//
// Cached per path with a coarse TTL: an os.Stat on every authenticated
// /health poll would give a token-thief a filesystem timing oracle on the
// host's binary layout, while the TTL still lets a re-deployed binary
// surface within one window.
func cliAvailable(path string) bool {
	return cliAvailableAt(path, time.Now())
}

// cliAvailableAt is the clock-injection seam for cliAvailable.
func cliAvailableAt(path string, now time.Time) bool {
	if v, ok := cliAvailCache.Load(path); ok {
		entry := v.(cliAvailEntry)
		if now.Sub(entry.generatedAt) < cliAvailCacheTTL {
			return entry.available
		}
	}
	_, err := os.Stat(path)
	available := err == nil
	cliAvailCache.Store(path, cliAvailEntry{generatedAt: now, available: available})
	return available
}

// cliAvailEntry is a single cache record; generatedAt carries the monotonic
// clock so host suspend cannot prematurely expire it.
type cliAvailEntry struct {
	generatedAt time.Time
	available   bool
}

// cliAvailCacheTTL caps how long a stat result is reused; tests may shorten it.
var cliAvailCacheTTL = 60 * time.Second

// cliAvailCache memoises cliAvailable(path) → cliAvailEntry, keyed by path.
var cliAvailCache sync.Map

// systemInfo returns compact system fingerprint for the workspace info bar.
// Cached after first call since values are static for the process lifetime.
//
// CONTRACT: the returned map is a process-wide singleton — callers MUST
// treat it as read-only (initStaticStats deep-copies before mutating).
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
