// Phase 5-prep / R-health-systeminfo-extract (2026-05-28):
// /health endpoint 用的系统反射 helpers 抽到独立文件。纯物理切分、
// 零行为变化。
//
// 这一组实现 handleHealth 的"读一次缓存一次"系统信息：
//   - cliAvailable / cliAvailableAt — CLI 二进制 stat 缓存（R247-SEC-21）
//   - cliAvailEntry / cliAvailCacheTTL / cliAvailCache — 缓存结构 + TTL
//   - systemInfo / sysInfoOnce / sysInfoVal — 进程级单例 OS/CPU/Mem 指纹
//   - localIPCount — 物理网卡 IPv4 数（不暴露具体地址）
//
// 与 *HealthHandler receiver 无关；调用方 handleHealth 通过同包可见性
// 继续使用，零改动。
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

// cliAvailable reports whether the CLI binary at path is stat-able. Extracted
// so handleHealth reads linearly without branching on `err != nil` for a single
// boolean — cleaner when the rest of the handler is struct initialization.
//
// R247-SEC-21: the result is cached per (path). The CLI binary path is set
// at process start and is effectively static; running os.Stat on every
// authenticated /health response (1 Hz × N tabs) gives a token-thief a
// precise filesystem-syscall oracle on the host's binary layout (timing of
// hot vs cold dentry cache differs measurably across reachable directories).
// Caching collapses every subsequent call to a wait-free load and makes the
// response time independent of host filesystem state.
//
// R250-SEC-3: a coarse TTL (cliAvailCacheTTL) is applied so a removed or
// re-deployed binary surfaces in /health within at most one TTL window
// instead of requiring a process restart. The window must be long enough
// that an attacker cannot use back-to-back stat calls as a precise oracle
// (60s mirrors the dashboard's other coarse caches) but short enough that
// a deploy-rotated binary becomes visible to operators within a minute.
func cliAvailable(path string) bool {
	return cliAvailableAt(path, time.Now())
}

// cliAvailableAt is the test seam: callers in production pass time.Now;
// tests inject a synthetic clock to exercise the TTL refresh path without
// sleeping. The seam keeps the public surface (cliAvailable) untouched.
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

// cliAvailEntry is a single cache record. generatedAt is monotonic-safe
// (time.Now() returns a wall+mono pair; Sub uses the mono component) so
// clock skew during host suspend cannot prematurely expire the cache.
type cliAvailEntry struct {
	generatedAt time.Time
	available   bool
}

// cliAvailCacheTTL caps how long a stat result is reused. Shared with the
// other dashboard read-only caches that prefer coarse refresh over per-call
// fs syscalls; 60s matches the existing convention. Test code may shadow
// this with a much shorter value via cliAvailCacheTTLForTest.
var cliAvailCacheTTL = 60 * time.Second

// cliAvailCache memoises cliAvailable(path) → cliAvailEntry. Keyed by path
// so a future caller with a different argument doesn't share another path's
// cached answer; in practice handleHealth always passes router.CLIPath()
// which is stable for the process lifetime.
var cliAvailCache sync.Map

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
