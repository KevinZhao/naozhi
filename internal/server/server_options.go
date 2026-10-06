// ServerOptions: the resolved-config view the Server constructor consumes.
package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/routerrelay"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/selfupdate"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
	transcribepkg "github.com/naozhi/naozhi/internal/transcribe"
)

// ServerOptions holds optional configuration for a Server.
// All fields have zero-value defaults (empty string, nil, zero duration = disabled/unset).
//
// Resolution boundary: every field is the post-Resolve view of config. The
// caller (cmd/naozhi/main.go) parses config.yaml, expands env vars, validates
// and materialises derived state before constructing this; Server.New never
// re-reads config or re-validates. New fields needing a derived value must
// take the derived form here, not the raw yaml shape (#681).
type ServerOptions struct {
	// Identity groups what the server reports about itself (S8, #2987).
	Identity    IdentityOptions
	AllowedRoot string // restricts /cd to paths under this root
	// IMAccess is the IM sender policy the dispatcher enforces; nil allows
	// every sender.
	IMAccess *imauth.Policy
	// IMRateLimit caps each IM sender's message rate; zero is unlimited.
	IMRateLimit dispatch.RateLimit
	// IMBudget refuses IM turns past cost.budget and answers /api/cost/budget;
	// nil admits every turn.
	IMBudget *budget.Gate
	// IMGroupScope is what one IM group-chat session covers; zero is per thread.
	IMGroupScope dispatch.GroupScope
	// IMThreadAutoOpen answers a group @mention outside any thread in a new
	// thread under it.
	IMThreadAutoOpen bool
	// StateDir is the only state directory the constructor owns end-to-end
	// (cookie_secret 0700/0600, retired-key ledger, size warning). Other state
	// dirs (~/.claude, workspace cwd, attachments, cron runs/shims) are owned
	// elsewhere (#407). Empty is legal: cookie secret becomes in-memory and
	// the retired-key store degrades to no-op.
	StateDir string
	// Watchdog groups the per-turn kill timeouts (S8, #2987).
	Watchdog WatchdogOptions
	// Queue groups the dispatch-queue knobs (#2553).
	Queue QueueOptions
	// Update groups the self-update surface (#2553).
	Update UpdateOptions
	// Sysession groups the system-daemon wiring (#2553).
	Sysession SysessionOptions
	// ImageOrient groups the image auto-orientation feature (#2553).
	ImageOrient ImageOrientOptions
	// Config groups the loaded-config identity and the paths derived from it
	// (#2553).
	Config ConfigOptions

	DashboardToken string // optional bearer token for dashboard API
	TrustedProxy   bool   // trust X-Forwarded-For for client IP
	ProjectManager *project.Manager
	// Remote groups the remote-node connections (S8, #2987).
	Remote      RemoteOptions
	Transcriber transcribepkg.Service
	// Lifecycle groups the startup hooks (S8, #2987).
	Lifecycle LifecycleOptions

	// Features groups the opt-in feature switches (S8, #2987).
	Features FeatureOptions

	// === Core dependencies ===
	//
	// The legacy New(addr, router, ..., opts) wrapper *overrides* matching
	// fields in opts with its positional args.
	Addr   string
	Router *session.Router
	// Relays groups the construction-cycle relays the server binds (S7/S8).
	Relays    RelayOptions
	Platforms map[string]platform.Platform
	// Routing groups the agent maps and the key resolver built over them.
	Routing   RoutingOptions
	Scheduler *cron.Scheduler
	Backend   string // "claude" | "kiro" | "" (empty → "claude")

	// Logger is the component logger the Server derives its structured logging
	// from. nil falls back to slog.Default() (#620).
	Logger *slog.Logger
}

// RoutingOptions are the agent maps and the KeyResolver the dispatcher, the
// Hub and the handlers share.
type RoutingOptions struct {
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	// Resolver is the process's shared resolver (wireup.KeyResolver), carrying
	// the cron access-profile lookup the remote gate needs. nil falls back to
	// a plain resolver over Agents and ProjectManager, with no cron lookup.
	Resolver *session.KeyResolver
}

// IdentityOptions is what the server reports about itself.
type IdentityOptions struct {
	WorkspaceID   string
	WorkspaceName string
	// Version is the build version string (the `-X main.version=...` ldflag).
	// Surfaced only on the authenticated part of /health and as `version_tag`
	// in /api/sessions stats; empty means unknown and /health omits the field.
	Version string
}

// WatchdogOptions are the per-turn kill timeouts the dispatcher enforces.
type WatchdogOptions struct {
	NoOutput time.Duration
	Total    time.Duration
}

// FeatureOptions are the opt-in feature switches.
type FeatureOptions struct {
	// Debug gates registration of /api/debug/pprof and /api/debug/vars.
	// Default false: both are 404 even for loopback+auth callers, so a leaked
	// dashboard token cannot enumerate goroutine stacks (file paths, queue
	// contents) or expvar counters. Set `server.debug_mode: true` only while
	// capturing a profile.
	Debug bool
	// Metrics registers GET /metrics (docs/ops/metrics.md).
	Metrics bool

	// PublicTmp opts the __public_tmp__ pseudo-project in (#646). When
	// false (default) that pseudo-project is a regular "project not found".
	//
	// SECURITY: MUST stay false on any shared / multi-operator deployment or
	// where the dashboard token is shared. When enabled every authenticated
	// dashboard user can read non-credential files anywhere under /tmp (the
	// credential allowlist and foreign-private-UID gate block secrets and
	// sockets, not general content). Accesses are audit-logged at Info
	// ("public_tmp file access") (#1678).
	PublicTmp bool

	// ProjectStableKey toggles the per-project StableKey field in the
	// /api/projects list response (docs/rfc/project-stable-session-key.md §4.2).
	// When false the dashboard falls back to the timestamp-key "continue" path.
	ProjectStableKey bool
}

// LifecycleOptions are the startup hooks.
type LifecycleOptions struct {
	// OnReady is called after the listener is bound and serving.
	OnReady func()
	// StartupCtx, when set, is threaded into blocking init probes (e.g. the
	// --version subprocess) so SIGTERM during startup aborts them promptly.
	// Nil is equivalent to context.Background().
	StartupCtx context.Context
}

// RemoteOptions are the remote-node connections.
type RemoteOptions struct {
	Nodes         map[string]node.Conn
	ReverseServer *node.ReverseServer
}

// RelayOptions are the relays the router, cron and sysession were built with;
// the server binds its consumers to them once they exist. nil leaves a relay
// unbound.
type RelayOptions struct {
	// Router is the relay Router was built with as its observer; the server
	// binds the dashboard's session-list and key-retirement consumers to it.
	Router *routerrelay.Relay
	// RunTelemetry is the relay cron and sysession were built with; the server
	// binds the Hub's run-event broadcaster to it.
	RunTelemetry *runtelemetry.Relay
}

// QueueOptions are the turn-queue knobs. Grouped out of the flat
// ServerOptions in #2553: they are set together from one config block and read
// only into turn.QueueOptions, which turn.New builds the queue from.
type QueueOptions struct {
	MaxDepth     int
	CollectDelay time.Duration
	Mode         string // "collect" (default) or "interrupt"; see turn.ParseMode
}

// UpdateOptions is the self-update surface behind /api/system/update.
type UpdateOptions struct {
	// Status is the shared self-update state the background selfupdate.Checker
	// writes into, surfaced by GET /api/system/update. Nil makes that endpoint
	// report only the running version.
	Status *selfupdate.Status
	// Checker owns Status; held so GET /api/system/update can trigger an
	// on-demand check during the cold-start window. Nil disables that
	// fallback; Status is still served.
	Checker *selfupdate.Checker
	// DashboardInstall gates POST /api/system/update/apply. nil defaults to
	// TRUE; an explicit false makes the endpoint 403 while the read-only GET
	// keeps working.
	DashboardInstall *bool
}

// SysessionOptions is the system-daemon wiring (docs/rfc/system-session.md).
type SysessionOptions struct {
	// Manager nil disables /api/system/* endpoints; the caller must
	// Manager.Start it before the server serves.
	Manager *sysession.Manager
	// WorkDir is the cwd sysession's Runner uses for transient `claude -p`
	// subprocesses. Session JSONLs under it are hidden from the catch-all
	// history panel (else AutoTitler prompts leak into "recent sessions").
	// Empty disables the filter.
	WorkDir string
}

// ImageOrientOptions is the image auto-orientation feature (docs: image_orient
// config).
type ImageOrientOptions struct {
	// Enabled gates the feature. Effective only when Runner is also non-nil;
	// otherwise POST /api/sessions/orient is a no-op returning rotated:false.
	Enabled bool
	// Model overrides --model on the side vision call. Empty uses the CLI
	// default; validated by config.validateModelString upstream.
	Model string
	// Runner is the image-capable one-off runner. nil disables the feature
	// regardless of Enabled.
	Runner VisionOrienter
}

// ConfigOptions is the loaded config's identity plus the paths derived from it.
type ConfigOptions struct {
	// SHA256 / LoadedAt are the loaded config's fingerprint
	// (config.Fingerprint, #2538), surfaced auth-only on /health so doctor and
	// the deploy playbook can detect "disk config changed after load — restart
	// required". Empty/zero when the caller built the config programmatically.
	SHA256   string
	LoadedAt time.Time
	// Live, when set, supersedes SHA256/LoadedAt on /health so a hot reload
	// is reflected without a restart (docs/rfc/config-hot-reload.md).
	Live *ConfigFingerprint
	// Reload re-reads the file and applies its hot sections; nil leaves
	// POST /api/system/config/reload answering 501.
	Reload ConfigReloadFunc
	// Path is the resolved path to config.yaml. Non-empty enables the
	// POST /api/access-profiles create endpoint (appends via yaml.Node
	// surgery); empty makes it return 400.
	Path string
	// AccessProfileSecretsDir is the trusted directory where the create
	// endpoint writes *_FILE token files (0600) as <dir>/<profileID>.token.
	// The id is charset-validated so the path cannot escape this dir. Empty
	// disables secret-file creation.
	AccessProfileSecretsDir string
}
