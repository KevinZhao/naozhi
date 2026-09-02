// preflight.go — "can this deployment actually apply an update?"
//
// The dashboard must not render a button that is guaranteed to fail. The
// classic case is a binary owned by root in /usr/local/bin while the service
// runs as an unprivileged user: Replace() cannot write there, so the honest UI
// is "run `sudo naozhi upgrade`" rather than a button that always errors.
//
// This is ADVISORY, not a gate. It is inherently TOCTOU — permissions can
// change between the check and the apply — so the authoritative error handling
// stays on Replace()'s return value. Preflight exists to shape the UI, and a
// positive result is not a promise of success.
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

// preflightTTL caches the filesystem probe. The dashboard polls this endpoint
// on a timer; without a cache every poll from every open browser would create
// and delete a file in the install directory.
const preflightTTL = 60 * time.Second

// probePattern is deliberately DIFFERENT from Replace()'s
// `.naozhi-upgrade-*.staging`. Sharing the pattern would put a probe file in
// the namespace a real concurrent install is using — and any future
// "clean up stale staging files" sweep would treat the other's file as debris.
const probePattern = ".naozhi-writeprobe-*"

var (
	preflightMu     sync.Mutex
	preflightCached *Preflight
	preflightAt     time.Time
	// preflightNow is indirected for tests that need to age the cache without
	// sleeping. Tests that swap it must not run in parallel, matching the
	// systemdUnitActive convention in this package.
	preflightNow = time.Now
)

// CheckPreflight evaluates applicability for the given action. Results are
// cached for preflightTTL, keyed by nothing — the inputs (platform, install
// dir permissions) are process-global.
//
// The `action` argument matters because the blocking conditions differ: an
// ActionRestart needs a manageable service but does NOT need a writable
// install directory (no bytes are written), while ActionInstall needs the
// directory and does not strictly need a running service.
//
// `serviceRunning` is passed IN rather than probed here, and the reason is cost,
// not style: on darwin ServiceRunning() shells out to `launchctl list`, and
// every caller already needs that same fact for its own response. Probing it
// here as well made the status endpoint fork three times per poll (this gate,
// the response's restart_supported field, and RollbackHint) for one answer that
// cannot change between them. Note it must stay a live read at the CALL site —
// caching it would make a just-installed launchd job invisible for a TTL, which
// on the POST path means refusing an apply that would in fact work.
func CheckPreflight(action Action, current string, serviceRunning bool) Preflight {
	if action == ActionNone {
		return Preflight{CanApply: false, Reason: ""}
	}

	preflightMu.Lock()
	defer preflightMu.Unlock()
	if preflightCached != nil && preflightNow().Sub(preflightAt) < preflightTTL {
		// The cached entry covers the expensive, action-independent probes.
		// Re-evaluate the action-specific gate below so switching from
		// staged→install inside one TTL window cannot serve a stale verdict.
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
		// Staged binary + no service we can drive = the operator has to
		// restart the process themselves. Nothing is broken; say so plainly.
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
	// A dev build has no released tag to compare against and replacing it
	// would silently discard a local build — the same rule checkOnce applies.
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

// writableInstallDir probes the install directory the same way Replace() will
// use it: create a file, then remove it.
//
// This is a real create rather than a permission-bit inspection (unix.Access
// or a FileMode check) on purpose — those miss read-only mounts, macOS SIP,
// and ACLs that deny write despite permissive mode bits. The probe is the only
// thing that answers the actual question.
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

// invalidatePreflight drops the cache. Called after an apply attempt so a
// permission change made in response to a failure is picked up immediately
// instead of after the TTL.
func invalidatePreflight() {
	preflightMu.Lock()
	preflightCached = nil
	preflightMu.Unlock()
}

// InvalidatePreflightCache is invalidatePreflight for callers outside this
// package. The cache key ignores `current` (in production it never changes), so
// a test that evaluates preflight for one version would otherwise leak its
// verdict into the next — call this between cases.
func InvalidatePreflightCache() { invalidatePreflight() }
