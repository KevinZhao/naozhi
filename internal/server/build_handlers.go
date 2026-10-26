package server

import (
	"context"
	"path/filepath"
	"time"

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

// buildCronHandlers constructs the dashboard cron handlers.
func buildCronHandlers(opts ServerOptions, claudeDir string) *dashcron.Handlers {
	// A nil Scheduler is the documented "cron disabled" state and dashcron
	// nil-guards it in 16 places, so the typed nil must not be boxed (#2561).
	var sched dashcron.SchedulerView
	if opts.Scheduler != nil {
		sched = opts.Scheduler
	}
	return dashcron.New(dashcron.Deps{
		Scheduler:        sched,
		AllowedRoot:      opts.AllowedRoot,
		ClaudeDir:        claudeDir,
		TranscriptSemCap: cronTranscriptSemCap,
		ValidateWS:       validateWorkspace,
		ClassifyWSErr:    classifyWorkspaceErr,
	})
}

// buildCostHandlers serves the cost ledger read API; the ledger may be
// nil/disabled (503).
func buildCostHandlers(opts ServerOptions, router *session.Router) *dashcost.Handlers {
	return dashcost.New(dashcost.Deps{
		Ledger: router.Runs().CostLedger(),
		Budget: opts.IMBudget,
	})
}

// cronTranscriptSemCap caps in-flight cron transcript reads; the audio
// transcribe path keeps its own semaphore.
const cronTranscriptSemCap = 8

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
		DefaultAgent:  opts.Routing.Agents["general"],
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
// endpoints.
func buildProjectHandlers(
	opts ServerOptions,
	nodeAccess *nodeRegistry,
	nodeCache *node.CacheManager,
) *dashproject.Handlers {
	// Typed-nil unwraps before the interface boxing (#2561). dashproject
	// nil-guards projectMgr and router because each is
	// optional; handing an interface field a nil CONCRETE pointer makes every
	// one of those guards read TRUE and the next call dereferences nil. The
	// concrete types are only visible here. Checklist for the next conversion:
	// grep the field's `!= nil` count BEFORE changing its type.
	var projectStore dashproject.ProjectStore
	if opts.ProjectManager != nil {
		projectStore = opts.ProjectManager
	}
	return dashproject.New(dashproject.Deps{
		ProjectMgr: projectStore,
		Router:     projectRouter{opts.Router},
		NodeAccess: nodeAccess,
		NodeCache:  nodeCache,
		// Process-local (resets on restart); not a filesystem quota.
		UploadQuotaBytes: defaultUploadQuotaBytes,

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
		ConfigReload:   system.ConfigReloader(opts.Config.Reload),
	})
}
