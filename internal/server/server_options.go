// Phase 5-prep (server-split-phase4-design.md §6.5 Plan B):
// ServerOptions moved out of server.go into its own file. Pure physical
// split, zero behaviour change. The struct is the resolved-config view
// the constructor consumes; field-level docs and the resolution-boundary
// note (R247-ARCH-23, #681) are preserved verbatim.
package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
	transcribepkg "github.com/naozhi/naozhi/internal/transcribe"
)

// ServerOptions holds optional configuration for a Server.
// All fields have zero-value defaults (empty string, nil, zero duration = disabled/unset).
//
// Resolution boundary (R247-ARCH-23, #681): every field below is the
// post-Resolve view of config. The caller (cmd/naozhi/main.go) is responsible
// for parsing config.yaml, expanding env vars, validating shape, and
// materialising any derived state (resolved AllowedRoot, picked Backend
// default, etc.) before constructing ServerOptions. Server.New therefore
// never re-reads config.yaml or re-runs validation — it consumes the
// resolved view as a stable input. Mirrors the RawConfig → ResolvedConfig
// split proposed for internal/config: any new server field that needs a
// derived value must take the derived form here, not the raw yaml shape.
type ServerOptions struct {
	WorkspaceID   string
	WorkspaceName string
	AllowedRoot   string // restricts /cd to paths under this root
	// StateDir is the only naozhi-state directory the server constructor
	// owns end-to-end: loadOrCreateCookieSecret writes cookie_secret here
	// (0700 mkdir + 0600 file), buildRetiredStoreWithErr persists the
	// retired-key ledger here, and warnIfStateDirLarge polls its size at
	// startup. The other state directories naozhi keeps on disk are owned
	// elsewhere by design — this comment anchors the split so a future
	// state.Layout aggregator (R214-ARCH-11, #407) lands without having
	// to rediscover the boundary:
	//
	//   - claudeDir (~/.claude)              → resolveClaudeDir() inside
	//                                          this constructor; consumed
	//                                          by takeover, history,
	//                                          discovery, transcript.
	//   - workspace cwd + storePath dir      → cmd/naozhi/main.go MkdirAlls
	//                                          before calling NewWithOptions
	//                                          and feeds them into Router.
	//   - attachment / upload subtree        → workspace-relative
	//                                          (.naozhi/attachments/),
	//                                          per-session at write time.
	//   - cron runs / shims                  → internal/cron + cli.Shim own
	//                                          their own subdirectories
	//                                          under the operator-supplied
	//                                          root; the server side only
	//                                          forwards paths.
	//
	// Empty StateDir is legal (test harnesses, ephemeral dev runs); the
	// cookie secret falls back to in-memory and the retired-key store
	// degrades to no-op. Operators get the canonical layout via the
	// `--state-dir` flag / config.yaml `server.state_dir` field.
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
	// StartupCtx, when set, is threaded into blocking init probes (e.g.
	// cli.DetectBackendsCtx's --version subprocess) so SIGTERM during naozhi
	// startup aborts them promptly instead of burning the full 5s×N
	// timeout. Nil is equivalent to context.Background() — safe default
	// for tests and callers that don't have a shutdown ctx yet.
	// R55-QUAL-004.
	StartupCtx context.Context
	// Version is the build version string (e.g. the `-X main.version=...`
	// ldflag value). When non-empty it is surfaced as `version` on the
	// authenticated portion of /health (R229-SEC-7 moved it out of the
	// unauthenticated probe section) and as `version_tag` inside
	// /api/sessions stats so the dashboard footer can render "v<tag>"
	// and authenticated probes can verify which build is live. Empty
	// value means "unknown" — /health omits the field, keeping the
	// legacy wire shape.
	Version string

	// DebugMode gates registration of /api/debug/pprof and /api/debug/vars.
	// Default false — both endpoints become 404 even for loopback+auth callers,
	// closing the residual surface where a leaked dashboard token plus host
	// access could enumerate goroutine stacks (which embed file paths +
	// queue contents) and expvar counters. Operators flip this to true via
	// `server.debug_mode: true` in config.yaml when they need to capture a
	// profile, then flip it back. R244-SEC-P3-1 [REPEAT-3].
	DebugMode bool

	// Headless declares that this Server is wired without a dashboard Hub on
	// purpose (test harnesses, headless tools that drive the send path
	// directly). It makes the nil-hub send fallback an explicit mode rather
	// than something inferred from `s.hub == nil`: with Headless=false (the
	// production default) Server.sendWithBroadcast fails loud when the hub is
	// missing, so a wiring regression panics at the send site instead of
	// silently routing through the no-broadcast fallback. R248-ARCH-9 (#379).
	Headless bool

	// PublicTmpEnabled opts the __public_tmp__ pseudo-project in (R237-SEC-5,
	// #646). When false (default) requests for that pseudo-project fall
	// through to the regular "project not found" surface — closes the
	// "any authed dashboard user can read /tmp" gap on multi-user
	// deployments. Single-operator dashboards (the typical naozhi use)
	// flip it on via `server.public_tmp_enabled: true` in config.yaml so
	// chat-mentioned /tmp/... paths still resolve without first
	// registering /tmp as a real project.
	PublicTmpEnabled bool

	// ProjectStableKeyEnabled toggles the per-project StableKey field in the
	// /api/projects list response (RFC docs/rfc/project-stable-session-key.md
	// §4.2). When false the field is omitted and the dashboard falls back to
	// the legacy timestamp-key path for "continue". Wired from
	// cfg.Session.ProjectStableKey.ResolvedEnabled(true).
	ProjectStableKeyEnabled bool

	// === Core dependencies (previously positional args of New) ===
	//
	// These fields were originally positional parameters on New(); they
	// now live in ServerOptions so a single constructor call can carry
	// the full config. NewWithOptions consumes them directly. The legacy
	// New(addr, router, ..., opts) wrapper still accepts positional args
	// and *overrides* matching fields in opts — a positional arg is
	// understood as "the caller is asserting this specific value, even
	// if they happened to leave a stale field in opts".
	Addr          string
	Router        *session.Router
	Platforms     map[string]platform.Platform
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	Scheduler     *cron.Scheduler
	Backend       string // "claude" | "kiro" | "" (empty → "claude")
	// SysessionManager is the system-daemon Manager (docs/rfc/system-session.md).
	// nil disables /api/system/* endpoints — Manager.Start should be invoked
	// by the caller (cmd/naozhi/main.go) before the server starts serving so
	// the inspector reads see live data on first poll.
	SysessionManager *sysession.Manager
	// SysWorkDir is the absolute filesystem path that sysession's Runner
	// uses as cwd for transient `claude -p` subprocesses (typically
	// <workspaceRoot>/sys-sessions). When set, every session JSONL whose
	// resolved workspace matches this path is hidden from the catch-all
	// history panel — without it AutoTitler prompt fragments leak into
	// the user's "recent sessions" list. Empty disables the filter
	// (matches the behaviour when sysession is disabled). R245-ARCH.
	SysWorkDir string

	// Logger is the component logger the Server derives all of its
	// structured logging from. When nil the Server falls back to
	// slog.Default() so existing callers (and the slog.SetDefault contract
	// in cmd/naozhi/main.go) keep working unchanged. Injecting a logger
	// here is the first concrete step of R247-ARCH-4 (#620): packages take
	// a *slog.Logger in their constructor and derive a component-scoped
	// child via slog.With("component", ...) instead of reading the process
	// global directly, which is what blocks t.Parallel across tests that
	// swap the default. SetDefault stays in main only for legacy callers.
	Logger *slog.Logger
}
