// ServerOptions: the resolved-config view the Server constructor consumes.
package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
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
	WorkspaceID   string
	WorkspaceName string
	AllowedRoot   string // restricts /cd to paths under this root
	// StateDir is the only state directory the constructor owns end-to-end
	// (cookie_secret 0700/0600, retired-key ledger, size warning). Other state
	// dirs (~/.claude, workspace cwd, attachments, cron runs/shims) are owned
	// elsewhere (#407). Empty is legal: cookie secret becomes in-memory and
	// the retired-key store degrades to no-op.
	StateDir          string
	NoOutputTimeout   time.Duration
	TotalTimeout      time.Duration
	QueueMaxDepth     int
	QueueCollectDelay time.Duration
	QueueMode         string // "collect" (default) or "interrupt"; see dispatch.ParseQueueMode
	DashboardToken    string // optional bearer token for dashboard API
	TrustedProxy      bool   // trust X-Forwarded-For for client IP
	ProjectManager    *project.Manager
	Nodes             map[string]node.Conn
	ReverseNodeServer *node.ReverseServer
	Transcriber       transcribepkg.Service
	OnReady           func() // called after the listener is bound and serving
	// StartupCtx, when set, is threaded into blocking init probes (e.g. the
	// --version subprocess) so SIGTERM during startup aborts them promptly.
	// Nil is equivalent to context.Background().
	StartupCtx context.Context
	// Version is the build version string (the `-X main.version=...` ldflag).
	// Surfaced only on the authenticated part of /health and as `version_tag`
	// in /api/sessions stats; empty means unknown and /health omits the field.
	Version string

	// UpdateStatus is the shared self-update state the background
	// selfupdate.Checker writes into, surfaced by GET /api/system/update.
	// Nil makes that endpoint report only the running version.
	UpdateStatus *selfupdate.Status

	// UpdateChecker is the Checker that owns UpdateStatus; held so
	// GET /api/system/update can trigger an on-demand check during the
	// cold-start window. Nil disables that fallback; Status is still served.
	UpdateChecker *selfupdate.Checker

	// UpdateDashboardInstall gates POST /api/system/update/apply.
	// nil defaults to TRUE; an explicit false makes the endpoint 403 while
	// the read-only GET keeps working.
	UpdateDashboardInstall *bool

	// DebugMode gates registration of /api/debug/pprof and /api/debug/vars.
	// Default false: both are 404 even for loopback+auth callers, so a leaked
	// dashboard token cannot enumerate goroutine stacks (file paths, queue
	// contents) or expvar counters. Set `server.debug_mode: true` only while
	// capturing a profile.
	DebugMode bool

	// Headless declares that this Server is wired without a dashboard Hub on
	// purpose. With Headless=false (production default) sendWithBroadcast
	// fails loud when the hub is missing instead of silently taking the
	// no-broadcast fallback (#379).
	Headless bool

	// PublicTmpEnabled opts the __public_tmp__ pseudo-project in (#646). When
	// false (default) that pseudo-project is a regular "project not found".
	//
	// SECURITY: MUST stay false on any shared / multi-operator deployment or
	// where the dashboard token is shared. When enabled every authenticated
	// dashboard user can read non-credential files anywhere under /tmp (the
	// credential allowlist and foreign-private-UID gate block secrets and
	// sockets, not general content). Accesses are audit-logged at Info
	// ("public_tmp file access") (#1678).
	PublicTmpEnabled bool

	// ProjectStableKeyEnabled toggles the per-project StableKey field in the
	// /api/projects list response (docs/rfc/project-stable-session-key.md §4.2).
	// When false the dashboard falls back to the timestamp-key "continue" path.
	ProjectStableKeyEnabled bool

	// === Core dependencies ===
	//
	// The legacy New(addr, router, ..., opts) wrapper *overrides* matching
	// fields in opts with its positional args.
	Addr          string
	Router        *session.Router
	Platforms     map[string]platform.Platform
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	Scheduler     *cron.Scheduler
	Backend       string // "claude" | "kiro" | "" (empty → "claude")
	// SysessionManager is the system-daemon Manager (docs/rfc/system-session.md).
	// nil disables /api/system/* endpoints; the caller must Manager.Start it
	// before the server serves.
	SysessionManager *sysession.Manager
	// SysWorkDir is the cwd sysession's Runner uses for transient `claude -p`
	// subprocesses. Session JSONLs under it are hidden from the catch-all
	// history panel (else AutoTitler prompts leak into "recent sessions").
	// Empty disables the filter.
	SysWorkDir string

	// Logger is the component logger the Server derives its structured logging
	// from. nil falls back to slog.Default() (#620).
	Logger *slog.Logger

	// === Image auto-orientation (docs: image_orient config) ===
	//
	// ImageOrientEnabled gates the feature. Effective only when
	// ImageOrientRunner is also non-nil; otherwise POST /api/sessions/orient
	// is a no-op returning rotated:false.
	ImageOrientEnabled bool
	// ImageOrientModel overrides --model on the side vision call. Empty uses
	// the CLI default; validated by config.validateModelString upstream.
	ImageOrientModel string
	// ImageOrientRunner is the image-capable one-off runner. nil disables the
	// feature regardless of ImageOrientEnabled.
	ImageOrientRunner VisionOrienter
	// ConfigPath is the resolved path to config.yaml. Non-empty enables the
	// POST /api/access-profiles create endpoint (appends via yaml.Node
	// surgery); empty makes it return 400.
	ConfigPath string
	// AccessProfileSecretsDir is the trusted directory where the create
	// endpoint writes *_FILE token files (0600) as <dir>/<profileID>.token.
	// The id is charset-validated so the path cannot escape this dir. Empty
	// disables secret-file creation.
	AccessProfileSecretsDir string
}
