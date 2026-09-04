// preflight.go — "can this deployment actually apply an update?" so the
// dashboard never renders a button guaranteed to fail (e.g. root-owned
// binary, unprivileged service). ADVISORY only and inherently TOCTOU: the
// authoritative error handling stays on Replace()'s return value.
package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Preflight reports whether an update can be applied and, when it cannot, a
// human-readable Chinese reason suitable for direct display.
type Preflight struct {
	CanApply bool
	Reason   string
}

// preflightTTL caches the filesystem probe so every browser poll does not
// create and delete a file in the install directory.
const preflightTTL = 60 * time.Second

// probePattern deliberately differs from Replace()'s staging pattern so a
// stale-staging sweep never treats a probe (or vice versa) as debris.
const probePattern = ".naozhi-writeprobe-*"

var (
	preflightMu     sync.Mutex
	preflightCached *Preflight
	preflightAt     time.Time
	// preflightNow is indirected so tests can age the cache; tests that swap
	// it must not run in parallel.
	preflightNow = time.Now
)

// CheckPreflight evaluates applicability for action; the action-independent
// probes are cached for preflightTTL. ActionRestart needs a manageable
// service but no writable dir; ActionInstall needs the dir. serviceRunning is
// passed in (a launchctl fork on darwin the caller already paid for) and must
// stay a live read at the call site so a just-installed job is not hidden for a TTL.
func CheckPreflight(action Action, current string, serviceRunning bool) Preflight {
	if action == ActionNone {
		return Preflight{CanApply: false, Reason: ""}
	}

	preflightMu.Lock()
	defer preflightMu.Unlock()
	if preflightCached != nil && preflightNow().Sub(preflightAt) < preflightTTL {
		// The action-specific gate is re-evaluated so a staged→install switch
		// inside one TTL cannot serve a stale verdict.
		if p := actionGate(action, serviceRunning); !p.CanApply {
			return p
		}
		return *preflightCached
	}

	p := computePreflight(current)
	preflightCached = &p
	preflightAt = preflightNow()
	if !p.CanApply {
		return p
	}
	if g := actionGate(action, serviceRunning); !g.CanApply {
		return g
	}
	return p
}

// actionGate holds the checks that depend on which action is being offered.
func actionGate(action Action, serviceRunning bool) Preflight {
	switch action {
	case ActionRestart:
		if !serviceRunning {
			return Preflight{
				CanApply: false,
				Reason:   "未检测到受管服务（systemd / launchd），手动重启进程即可让新版本生效",
			}
		}
	case ActionInstall:
		if !writableInstallDir() {
			dir := installDir()
			return Preflight{
				CanApply: false,
				Reason: fmt.Sprintf("%s 不可写（当前 uid %d）：请用 sudo naozhi upgrade 手动升级",
					dir, os.Getuid()),
			}
		}
	}
	return Preflight{CanApply: true}
}

// computePreflight runs the action-independent checks.
func computePreflight(current string) Preflight {
	if current == "dev" || current == "" {
		return Preflight{
			CanApply: false,
			Reason:   "dev build：本地构建不会被自动替换，需要时用 naozhi upgrade --force",
		}
	}
	if err := checkPlatform(); err != nil {
		return Preflight{
			CanApply: false,
			Reason:   fmt.Sprintf("当前平台（%s/%s）没有对应的 release 资产", runtime.GOOS, runtime.GOARCH),
		}
	}
	return Preflight{CanApply: true}
}

// installDir returns the directory holding the running binary, or "" when it
// cannot be resolved.
func installDir() string {
	p, err := SelfPath()
	if err != nil {
		return ""
	}
	return filepath.Dir(p)
}

// writableInstallDir probes by creating and removing a file, not by inspecting
// mode bits — those miss read-only mounts, macOS SIP and ACLs.
func writableInstallDir() bool {
	dir := installDir()
	if dir == "" {
		return false
	}
	f, err := os.CreateTemp(dir, probePattern)
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// invalidatePreflight drops the cache after an apply attempt so a permission
// fix is picked up immediately.
func invalidatePreflight() {
	preflightMu.Lock()
	preflightCached = nil
	preflightMu.Unlock()
}

// InvalidatePreflightCache is invalidatePreflight for other packages' tests:
// the cache ignores `current`, so verdicts would otherwise leak between cases.
func InvalidatePreflightCache() { invalidatePreflight() }
