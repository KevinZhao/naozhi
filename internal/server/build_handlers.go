package server

import (
	"context"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	dashcron "github.com/naozhi/naozhi/internal/dashboard/cron"
	dashdiscovery "github.com/naozhi/naozhi/internal/dashboard/discovery"
	"github.com/naozhi/naozhi/internal/dashboard/ext/transcribe"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"golang.org/x/time/rate"
)

// build_handlers.go is part of the buildServer split (R246-CR-004 / #738).
// Each helper constructs one handler-struct group from ServerOptions plus
// already-resolved derived state (cookieSecret, claudeDir, agents, etc.).
// The helpers are kept package-private and have no side effects on the
// Server value — buildServer remains the single owner of struct assembly,
// it just delegates the per-domain literal blocks here so the constructor
// reads as a contents-of-Server outline rather than a 358-line wall.
//
// Naming: build<Domain>Handlers returns the handler pointer. When the
// helper needs already-constructed Server fields (e.g. nodeAccess) the
// caller passes them as explicit parameters — no helper accepts a
// partially-constructed *Server, which keeps initialization order
// inspectable at the buildServer call site.

// buildAuthHandlers constructs the AuthHandlers shared by login + WS
// upgrade paths. Limiter buckets are sized to fit human refresh cadence
// (60/min sustained, 20 burst); see AuthHandlers commentary at field-decl
// site for the exhaustive justification. unauthDashLimiter intentionally
// reuses the same shape as wsUpgradeLimiter — same scanner-blocking
// envelope works for both unauthenticated dashboard probes and WS
// upgrades. R230C-SEC-12.
func buildAuthHandlers(opts ServerOptions, cookieSecret []byte, cookieGen string) *auth.Handlers {
	return auth.New(opts.DashboardToken, cookieSecret, cookieGen, opts.TrustedProxy)
}

// buildCronHandlers constructs CronHandlers with the three per-IP limiters
// gating cron endpoints:
//
//   - runsLimiter (R222-SEC-3): /api/cron/runs and /api/cron/runs/{run_id}.
//     60 req/min/IP with burst 60 mirrors the per-minute pace the
//     dashboard uses when paginating run history (one initial fetch +
//     occasional refresh) and leaves enough headroom for the run-detail
//     drawer to fan out a few sequential reads. A stolen token can
//     otherwise enumerate the entire on-disk run history at unbounded
//     rate, both burning IO and exposing per-job activity timing.
//
//   - listLimiter (R242-CR-3): the 1 Hz GET /api/cron poll. Dashboard
//     tabs hit this endpoint roughly once per second each, and the
//     per-call cost is O(N jobs × RecentRuns(5)) of sync.Map loads +
//     entry locks — cheap individually but unbounded under hostile
//     parallelism. 2 req/s sustained with burst 30 leaves plenty of
//     headroom for legit dashboard refresh bursts (tab switch + filter
//     change) while capping a stolen token's steady-state poll rate.
//
//   - writeLimiter (R247-SEC-2 / R247-SEC-3): cron write/control endpoints
//     (trigger, preview). 30 req/min sustained with burst 6 — legitimate
//     UI form-edit loops hit preview a handful of times per minute, while
//     a stolen token is capped at one trigger every 2 s steady-state.
//
// R242-SEC-8 / #636: all three buckets are constructed via
// newIPLimiterWithCap so the per-limiter LRU cap and idle TTL are pinned
// explicitly rather than inherited from the ratelimit package defaults
// (1000 / 10m). The implicit cap is a DDoS soft floor — once 1000 fresh
// attacker-IPs land, the LRU evicts the oldest legit rate-limited entries
// and they come back unthrottled. cronLimiterMaxKeys raises that floor
// to 8192 and cronLimiterTTL pins idle expiry at 5 minutes, which is
// well above the 1 Hz poll cadence yet short enough that a transient
// scanner does not occupy a slot for the full 10-minute default.
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

// cronTranscriptSemCap caps in-flight cron transcript reads. Renamed from
// server-package transcribeSemCap to disambiguate; the audio transcribe
// path keeps its own semaphore. Phase 1.
const cronTranscriptSemCap = 8

// cronLimiterMaxKeys / cronLimiterTTL pin the LRU cap + idle TTL for the
// three cron-handler limiters. R242-SEC-8 / #636: previously the buckets
// inherited ratelimit.New defaults (MaxKeys=1000 / TTL=10m). Under DDoS
// — bursts of fresh attacker IPs spread across XFF-spoofed sources —
// the 1000-key LRU saturates and the *oldest* entry is evicted to make
// room. The evicted entry is by definition a legitimate rate-limited IP
// (it's been in the bucket long enough to be the LRU tail), so it loses
// its accumulated debt and comes back un-throttled on the next request.
//
// Raising MaxKeys to 8192 lifts the saturation floor 8× (≈1 MiB worst-
// case for the three cron buckets combined; cheap given a heartbeat-path
// memory budget). The 5-minute TTL is shorter than the 10-minute default
// — at 1 Hz dashboard cadence a legit poller refreshes its lastSeen on
// every tick, so anything idle for 5 minutes is either a tab that closed
// or a one-shot scanner; either way evicting it frees the slot for new
// active callers without disturbing rate-limited regulars.
const (
	cronLimiterMaxKeys = 8192
	cronLimiterTTL     = 5 * time.Minute
)

// buildTranscribeHandler constructs the speech-to-text handler with a
// per-IP rate limiter (5/min) and a fixed-cap concurrency semaphore.
// Both backstops are defence-in-depth: a stolen token plus a large audio
// payload would otherwise drive unbounded CPU + outbound API spend on
// whichever transcribe service is wired.
func buildTranscribeHandler(opts ServerOptions) *transcribe.Handler {
	return transcribe.New(transcribe.Deps{
		Transcriber: opts.Transcriber,
		Limiter:     newIPLimiterWithProxy(rate.Every(12*time.Second), 5, opts.TrustedProxy),
		SemCap:      transcribe.TranscribeSemCap,
	})
}

// buildRetiredStoreWithErr constructs the discovery.RetiredStore eagerly so
// the SessionHandlers can hold a non-nil pointer at construction time. When
// stateDir is set the store is persisted to <stateDir>/history-retired.json;
// otherwise an in-memory-only store is returned (tests, ephemeral
// deployments). The (store, err) shape lets buildServer log a slog.Warn
// with the underlying disk error — a corrupt file just means the popover
// starts with last_active sort, but operators want the cause in journals.
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
//
// Phase 3b (server-split-phase4-design.md §6.5 Plan B): the handler group
// lives in internal/dashboard/discovery; this helper bridges server-package
// private types (validateWorkspace, verifyProcIdentity, *session.Router)
// into the small interface surface the sub-package consumes.
func buildDiscoveryHandlers(
	opts ServerOptions,
	claudeDir string,
	cache *discoveryCache,
	nodeAccess *nodeAccessor,
	nodeCache *node.CacheManager,
	broadcast func(),
) *dashdiscovery.Handlers {
	return dashdiscovery.New(dashdiscovery.Deps{
		Cache:        cache,
		NodeAccess:   nodeAccess,
		NodeCache:    nodeCache,
		ClaudeDir:    claudeDir,
		Router:       routerTakeoverAdapter{r: opts.Router},
		AllowedRoot:  opts.AllowedRoot,
		DefaultAgent: opts.Agents["general"],
		Broadcast:    broadcast,
		ValidateWS:   validateWorkspace,
		VerifyProcID: verifyProcIdentity,
	})
}

// routerTakeoverAdapter narrows *session.Router's Takeover return shape
// (`*ManagedSession, error`) to the `error`-only signature the discovery
// sub-package consumes. Discovery handlers ignore the *ManagedSession; the
// adapter discards it here so the interface in dashdiscovery.SessionRouter
// can stay tiny and not transitively re-export internal/session types.
type routerTakeoverAdapter struct{ r *session.Router }

func (a routerTakeoverAdapter) Takeover(ctx context.Context, key, sessionID, cwd string, opts session.AgentOpts) error {
	_, err := a.r.Takeover(ctx, key, sessionID, cwd, opts)
	return err
}

// buildProjectHandlers wires the dashboard project-config + project-files
// endpoints. Two per-IP limiters are kept tighter than the cron set
// because both paths touch disk on every call:
//
//   - filesExistsLimiter (S13): /api/projects/files/exists. 10/min matches
//     the uploadLimiter cadence — both endpoints do filesystem I/O and
//     belong to the same DoS class. Burst 10 accommodates the dashboard's
//     initial batch-render pass that can spawn several exists calls
//     back-to-back when a session is opened with many file references.
//
//   - configPutLimiter (R247-SEC-7): PUT /api/projects/config. The handler
//     persists ProjectConfig to disk and broadcasts a WS update to every
//     subscribed dashboard client; without a gate any authenticated caller
//     can drive unbounded disk + fan-out. 5/sec burst 5 ≈ 5×60=300/min —
//     well above interactive editing (a single user saves config sub-
//     second after each edit) but well below abuse rates a script could
//     reach.
//
// R247-ARCH-15 (#650): ctxFunc closure parameter retired. The handler
// now stores `baseCtx` as a plain field that registerDashboard wires
// via SetBaseContext once `s.hub` exists. The two-phase wiring is
// unchanged (Hub still doesn't exist when buildProjectHandlers is
// called); only the DI shape moved from a captured closure to a
// direct field assign.
func buildProjectHandlers(
	opts ServerOptions,
	resolver *session.KeyResolver,
	nodeAccess *nodeAccessor,
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
		PublicTmpEnabled:   opts.PublicTmpEnabled,

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
