package server

import (
	"context"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/dashboard/auth"
	dashcost "github.com/naozhi/naozhi/internal/dashboard/cost"
	dashcron "github.com/naozhi/naozhi/internal/dashboard/cron"
	dashdiscovery "github.com/naozhi/naozhi/internal/dashboard/discovery"
	"github.com/naozhi/naozhi/internal/dashboard/ext/system"
	"github.com/naozhi/naozhi/internal/dashboard/ext/transcribe"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
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
	// A nil Scheduler is the documented "cron disabled" state and dashcron
	// nil-guards it in 16 places, so the typed nil must not be boxed (#2561).
	var sched dashcron.SchedulerView
	if opts.Scheduler != nil {
		sched = opts.Scheduler
	}
	return dashcron.New(dashcron.Deps{
		Scheduler:   sched,
		AllowedRoot: opts.AllowedRoot,
		ClaudeDir:   claudeDir,
		RateLimits: dashcron.RateLimits{
			Runs: newIPLimiterWithCap(
				rate.Every(time.Second), 60,
				cronLimiterMaxKeys, cronLimiterTTL, opts.TrustedProxy,
			),
			List: newIPLimiterWithCap(
				rate.Every(500*time.Millisecond), 30,
				cronLimiterMaxKeys, cronLimiterTTL, opts.TrustedProxy,
			),
			Write: newIPLimiterWithCap(
				rate.Every(2*time.Second), 6,
				cronLimiterMaxKeys, cronLimiterTTL, opts.TrustedProxy,
			),
			Transcript: newIPLimiterWithCap(
				rate.Every(10*time.Second), 12,
				cronLimiterMaxKeys, cronLimiterTTL, opts.TrustedProxy,
			),
		},
		TranscriptSemCap: cronTranscriptSemCap,
		ValidateWS:       validateWorkspace,
		ClassifyWSErr:    classifyWorkspaceErr,
	})
}

// buildCostHandlers serves the cost ledger read API at the dashboard's 1 Hz
// poll budget (2/s, burst 30); the ledger may be nil/disabled (503).
func buildCostHandlers(opts ServerOptions, router *session.Router) *dashcost.Handlers {
	var ledger *costledger.Store
	if router != nil {
		ledger = router.Runs().CostLedger()
	}
	return dashcost.New(dashcost.Deps{
		Ledger: ledger,
		Limiter: newIPLimiterWithCap(
			rate.Every(500*time.Millisecond), 30,
			cronLimiterMaxKeys, cronLimiterTTL, opts.TrustedProxy,
		),
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
	appCtx context.Context,
) *dashdiscovery.Handlers {
	return dashdiscovery.New(dashdiscovery.Deps{
		AppCtx:        appCtx,
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

// routerTakeoverAdapter narrows *session.Router's takeover to the shapes the
// discovery sub-package consumes (an error-only Takeover on an interface
// lease), so that package need not re-export session types.
type routerTakeoverAdapter struct{ r *session.Router }

func (a routerTakeoverAdapter) ReserveTakeover(key string, opts session.AgentOpts) (dashdiscovery.TakeoverLease, error) {
	lease, err := a.r.ReserveTakeover(key, opts)
	if err != nil {
		return nil, err
	}
	return takeoverLeaseAdapter{a.r, lease}, nil
}

type takeoverLeaseAdapter struct {
	r     *session.Router
	lease *session.TakeoverLease
}

func (a takeoverLeaseAdapter) Takeover(ctx context.Context, sessionID, cwd string) error {
	_, err := a.r.Takeover(ctx, a.lease, sessionID, cwd)
	return err
}

func (a takeoverLeaseAdapter) Release() { a.lease.Release() }

// buildProjectHandlers wires the dashboard project-config + project-files
// endpoints. Both per-IP limiters are tighter than the cron set because both
// paths touch disk on every call: files/exists 10/min burst 10 (same DoS
// class as upload); PUT config 5/s burst 5 (persists to disk + WS fan-out).
// The Hub does not exist yet at this point; registerDashboard wires the
// base context at construction via Deps.BaseCtx (#650, #2552).
func buildProjectHandlers(
	opts ServerOptions,
	resolver *session.KeyResolver,
	nodeAccess *nodeRegistry,
	nodeCache *node.CacheManager,
	baseCtx context.Context,
) *dashproject.Handlers {
	// Typed-nil unwraps before the interface boxing (#2561). dashproject
	// nil-guards projectMgr (×9), router (×2) and resolver (×1) because each is
	// optional; handing an interface field a nil CONCRETE pointer makes every
	// one of those guards read TRUE and the next call dereferences nil. The
	// concrete types are only visible here. Checklist for the next conversion:
	// grep the field's `!= nil` count BEFORE changing its type.
	var projectStore dashproject.ProjectStore
	if opts.ProjectManager != nil {
		projectStore = opts.ProjectManager
	}
	var projectRouterView dashproject.RouterView
	if opts.Router != nil {
		projectRouterView = projectRouter{opts.Router}
	}
	var plannerResolver dashproject.PlannerKeyResolver
	if resolver != nil {
		plannerResolver = resolver
	}
	return dashproject.New(dashproject.Deps{
		BaseCtx:            baseCtx,
		ProjectMgr:         projectStore,
		Router:             projectRouterView,
		Resolver:           plannerResolver,
		NodeAccess:         nodeAccess,
		NodeCache:          nodeCache,
		FilesExistsLimiter: newIPLimiterWithProxy(rate.Every(6*time.Second), 10, opts.TrustedProxy),
		ConfigPutLimiter:   newIPLimiterWithProxy(rate.Every(200*time.Millisecond), 5, opts.TrustedProxy),
		// Process-local (resets on restart); not a filesystem quota.
		UploadQuotaBytes: defaultUploadQuotaBytes,
		PublicTmpEnabled: opts.Features.PublicTmp,

		ProjectStableKeyEnabled: opts.Features.ProjectStableKey,
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

// buildSystemHandlers constructs the /api/system/* group. A nil
// SysessionManager or update Checker must become a nil interface, not a
// non-nil interface wrapping nil, or the endpoints' disabled paths never fire.
// The Status goes in as-is: *selfupdate.Status is nil-receiver safe, and the
// handlers call it unguarded.
func buildSystemHandlers(opts ServerOptions, router *session.Router) *system.Handlers {
	var daemons system.DaemonInspector
	if opts.Sysession.Manager != nil {
		daemons = opts.Sysession.Manager
	}
	var checker system.UpdateChecker
	if opts.Update.Checker != nil {
		checker = opts.Update.Checker
	}
	return system.New(system.Deps{
		Daemons:       daemons,
		Router:        router,
		UpdateStatus:  opts.Update.Status,
		UpdateChecker: checker,
		BuildVersion:  opts.Identity.Version,
		// nil ⇒ enabled, matching config.UpdateDashboardInstall's default.
		InstallEnabled: opts.Update.DashboardInstall == nil || *opts.Update.DashboardInstall,
	})
}
