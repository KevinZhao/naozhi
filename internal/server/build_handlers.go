package server

import (
	"context"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	dashcron "github.com/naozhi/naozhi/internal/dashboard/cron"
	dashdiscovery "github.com/naozhi/naozhi/internal/dashboard/discovery"
	"github.com/naozhi/naozhi/internal/dashboard/ext/system"
	"github.com/naozhi/naozhi/internal/dashboard/ext/transcribe"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"golang.org/x/time/rate"
)

// defaultUploadQuotaBytes caps cumulative per-project upload bytes per process
// so one tenant cannot fill a shared disk through the upload endpoint (#2311).
// 4 GiB ≈ 16 max-size (256 MiB) files.
const defaultUploadQuotaBytes int64 = 4 << 30

// Each build<Domain>Handlers helper constructs one handler group from
// ServerOptions plus already-resolved derived state. No helper accepts a
// partially-constructed *Server — needed fields are passed explicitly so
// initialization order stays inspectable at the buildServer call site (#738).

// buildAuthHandlers constructs the AuthHandlers shared by login + WS
// upgrade paths.
func buildAuthHandlers(opts ServerOptions, cookieSecret []byte, cookieGen string) *auth.Handlers {
	return auth.New(opts.DashboardToken, cookieSecret, cookieGen, opts.TrustedProxy)
}

// buildCronHandlers constructs CronHandlers with per-IP limiters gating the
// cron endpoints: runs (1/s, burst 60 — a stolen token must not enumerate
// the whole run history), list (2/s, burst 30 — the 1 Hz dashboard poll),
// write/trigger/preview (1 per 2s, burst 6), transcript. All use
// newIPLimiterWithCap so the LRU cap / idle TTL are pinned explicitly
// (see cronLimiterMaxKeys).
func buildCronHandlers(opts ServerOptions, claudeDir string) *dashcron.Handlers {
	return dashcron.New(dashcron.Deps{
		Scheduler:   opts.Scheduler,
		AllowedRoot: opts.AllowedRoot,
		ClaudeDir:   claudeDir,
		RunsLimiter: newIPLimiterWithCap(
			rate.Every(time.Second), 60,
			cronLimiterMaxKeys, cronLimiterTTL, opts.TrustedProxy,
		),
		ListLimiter: newIPLimiterWithCap(
			rate.Every(500*time.Millisecond), 30,
			cronLimiterMaxKeys, cronLimiterTTL, opts.TrustedProxy,
		),
		WriteLimiter: newIPLimiterWithCap(
			rate.Every(2*time.Second), 6,
			cronLimiterMaxKeys, cronLimiterTTL, opts.TrustedProxy,
		),
		TranscriptLimiter: newIPLimiterWithCap(
			rate.Every(10*time.Second), 12,
			cronLimiterMaxKeys, cronLimiterTTL, opts.TrustedProxy,
		),
		TranscriptSemCap: cronTranscriptSemCap,
		ValidateWS:       validateWorkspace,
		ClassifyWSErr:    classifyWorkspaceErr,
	})
}

// cronTranscriptSemCap caps in-flight cron transcript reads; the audio
// transcribe path keeps its own semaphore.
const cronTranscriptSemCap = 8

// cronLimiterMaxKeys / cronLimiterTTL pin the LRU cap + idle TTL for the
// cron-handler limiters. A small LRU is a DDoS soft floor: a burst of fresh
// (XFF-spoofed) IPs evicts the oldest — i.e. legitimately rate-limited —
// entries, which come back unthrottled. 8192 keys ≈ 1 MiB worst case; 5m
// idle is well above the 1 Hz poll cadence (#636).
const (
	cronLimiterMaxKeys = 8192
	cronLimiterTTL     = 5 * time.Minute
)

// buildTranscribeHandler constructs the speech-to-text handler with a
// per-IP rate limiter (5/min) and a fixed-cap concurrency semaphore, so a
// stolen token cannot drive unbounded CPU + outbound API spend.
func buildTranscribeHandler(opts ServerOptions) *transcribe.Handler {
	return transcribe.New(transcribe.Deps{
		Transcriber: opts.Transcriber,
		Limiter:     newIPLimiterWithProxy(rate.Every(12*time.Second), 5, opts.TrustedProxy),
		SemCap:      transcribe.TranscribeSemCap,
	})
}

// buildRetiredStoreWithErr constructs the discovery.RetiredStore eagerly so
// the SessionHandlers can hold a non-nil pointer at construction time.
// Persisted to <stateDir>/history-retired.json when stateDir is set, else
// in-memory. The err lets buildServer log a corrupt file (the store still works).
func buildRetiredStoreWithErr(stateDir string) (*discovery.RetiredStore, error) {
	if stateDir == "" {
		store, _ := discovery.NewRetiredStore("")
		return store, nil
	}
	return discovery.NewRetiredStore(filepath.Join(stateDir, "history-retired.json"))
}

// buildDiscoveryHandlers wires the local-discovery + node-cache sources
// behind the dashboard discovery endpoints. broadcast is invoked when the
// cache observes a change so subscribed dashboard clients receive fresh
// state without a manual refresh.
func buildDiscoveryHandlers(
	opts ServerOptions,
	claudeDir string,
	cache *discoveryCache,
	nodeAccess *nodeRegistry,
	nodeCache *node.CacheManager,
	broadcast func(),
) *dashdiscovery.Handlers {
	return dashdiscovery.New(dashdiscovery.Deps{
		Cache:         cache,
		NodeAccess:    nodeAccess,
		NodeCache:     nodeCache,
		ClaudeDir:     claudeDir,
		Router:        routerTakeoverAdapter{r: opts.Router},
		AllowedRoot:   opts.AllowedRoot,
		DefaultAgent:  opts.Agents["general"],
		Broadcast:     broadcast,
		ValidateWS:    validateWorkspace,
		VerifyProcID:  verifyProcIdentity,
		ProcStartTime: discovery.ProcStartTime,
	})
}

// routerTakeoverAdapter narrows *session.Router's Takeover return shape
// (`*ManagedSession, error`) to the `error`-only signature the discovery
// sub-package consumes, so that interface need not re-export session types.
type routerTakeoverAdapter struct{ r *session.Router }

func (a routerTakeoverAdapter) Takeover(ctx context.Context, key, sessionID, cwd string, opts session.AgentOpts) error {
	_, err := a.r.Takeover(ctx, key, sessionID, cwd, opts)
	return err
}

// buildProjectHandlers wires the dashboard project-config + project-files
// endpoints. Both per-IP limiters are tighter than the cron set because both
// paths touch disk on every call: files/exists 10/min burst 10 (same DoS
// class as upload); PUT config 5/s burst 5 (persists to disk + WS fan-out).
// The Hub does not exist yet at this point; registerDashboard wires the
// base context later via SetBaseContext (#650).
func buildProjectHandlers(
	opts ServerOptions,
	resolver *session.KeyResolver,
	nodeAccess *nodeRegistry,
	nodeCache *node.CacheManager,
) *dashproject.Handlers {
	return dashproject.New(dashproject.Deps{
		ProjectMgr:         opts.ProjectManager,
		Router:             opts.Router,
		Resolver:           resolver,
		NodeAccess:         nodeAccess,
		NodeCache:          nodeCache,
		FilesExistsLimiter: newIPLimiterWithProxy(rate.Every(6*time.Second), 10, opts.TrustedProxy),
		ConfigPutLimiter:   newIPLimiterWithProxy(rate.Every(200*time.Millisecond), 5, opts.TrustedProxy),
		// Process-local (resets on restart); not a filesystem quota.
		UploadQuotaBytes: defaultUploadQuotaBytes,
		PublicTmpEnabled: opts.PublicTmpEnabled,

		ProjectStableKeyEnabled: opts.ProjectStableKeyEnabled,
	})
}

// agentIDList returns ["general"] followed by the configured agent IDs.
// "general" is always first because the dashboard treats it as the
// fallback agent when the saved selection no longer exists.
func agentIDList(agents map[string]session.AgentOpts) []string {
	ids := make([]string, 0, len(agents)+1)
	ids = append(ids, "general")
	for id := range agents {
		if id == "general" {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// platformNameSet returns the set of platform names registered with the
// server. HealthHandler exposes this as a static `platforms` field on
// /health so probes don't need to walk the live map.
func platformNameSet(platforms map[string]platform.Platform) map[string]struct{} {
	out := make(map[string]struct{}, len(platforms))
	for name := range platforms {
		out[name] = struct{}{}
	}
	return out
}

// platformStatusMap pre-builds the {name: "registered"} map HealthHandler
// serves as the /health `platforms` sub-object (registration is fixed at
// construction).
func platformStatusMap(names map[string]struct{}) map[string]string {
	out := make(map[string]string, len(names))
	for name := range names {
		out[name] = "registered"
	}
	return out
}

// buildSystemHandlers constructs the /api/system/* group. A nil
// SysessionManager must become a nil interface, not a non-nil interface
// wrapping nil, or the daemons endpoint's disabled path never fires.
func buildSystemHandlers(opts ServerOptions, router *session.Router) *system.Handlers {
	var daemons system.DaemonInspector
	if opts.SysessionManager != nil {
		daemons = opts.SysessionManager
	}
	return system.New(system.Deps{
		Daemons:       daemons,
		Router:        router,
		UpdateStatus:  opts.UpdateStatus,
		UpdateChecker: opts.UpdateChecker,
		BuildVersion:  opts.Version,
		// nil ⇒ enabled, matching config.UpdateDashboardInstall's default.
		InstallEnabled: opts.UpdateDashboardInstall == nil || *opts.UpdateDashboardInstall,
	})
}
