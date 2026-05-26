package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
	"github.com/naozhi/naozhi/internal/transcribe"
)

const (
	defaultDedupCapacity = 10000

	// maxRequestBodyBytes is the per-handler request-body read limit applied
	// via http.MaxBytesReader. 1 MiB is well above the largest JSON payload
	// any handler legitimately accepts, but safely below typical DoS-attempt
	// sizes. All dashboard mutation handlers must use this constant so the
	// limit is adjusted in one place.
	maxRequestBodyBytes = 1 << 20
)

// Server is the HTTP entry point for Naozhi.
//
// Field-block contract (server-split-phase4-design.md §五 / §六.6):
// Each field below carries `// 读写: <files>` to indicate which non-test
// files in this package access it. New fields MUST add this annotation.
// Phase 4-5 will redistribute most of these into wshub Options, dashboard
// sub-packages, or routes.go locals. Verification rule:
//
//	awk '/^type Server struct/,/^}$/' server.go | grep -cE '^\s+[a-zA-Z_]+ '
//
// must equal the field count documented in
// docs/design/server-split-phase4-baseline.md §2 (currently 47).
type Server struct {
	// ── HTTP entry (Phase 5: keep) ─────────────────────
	addr      string          // 读写: server.go
	mux       *http.ServeMux  // 读写: server.go, dashboard.go, debug_expvar.go, debug_pprof.go
	startedAt time.Time       // 读写: server.go
	onReady   func()          // 读写: server.go (called after listener is bound)
	appCtx    context.Context // 读写: server.go, dashboard.go (HubOptions.ParentCtx)

	// ── core deps (Phase 5: keep) ──────────────────────
	router     *session.Router  // 读写: server.go, dashboard.go, dashboard_system.go, send.go, takeover.go, consumer.go
	scheduler  *cron.Scheduler  // 读写: server.go, dashboard.go, dashboard_cron.go, dashboard_cron_transcript.go, wshub.go
	hub        *Hub             // 读写: server.go, dashboard.go, send.go (WebSocket hub)
	projectMgr *project.Manager // 读写: server.go, dashboard.go, project_api.go, project_files.go

	// ── multi-node (Phase 5: keep) ─────────────────────
	nodes             map[string]node.Conn // 读写: server.go, dashboard.go
	reverseNodeServer *node.ReverseServer  // 读写: server.go, dashboard.go
	nodesMu           sync.RWMutex         // 读写: server.go, dashboard.go (shared with Hub.nodesMu)

	// ── Phase 5: → routes.go local variables ───────────
	auth         *AuthHandlers        // 读写: server.go, dashboard.go, debug_expvar.go, debug_pprof.go
	cronH        *CronHandlers        // 读写: server.go, dashboard.go
	transcribeH  *TranscribeHandler   // 读写: dashboard.go (ctor only in server.go)
	nodeAccess   *nodeAccessor        // 读写: server.go, dashboard.go
	discoveryH   *DiscoveryHandlers   // 读写: server.go, dashboard.go
	projectH     *ProjectHandlers     // 读写: server.go, dashboard.go
	sessionH     *SessionHandlers     // 读写: server.go, dashboard.go
	healthH      *HealthHandler       // 读写: server.go (ctor only)
	sendH        *SendHandler         // 读写: dashboard.go (ctor only in server.go)
	cliH         *CLIBackendsHandler  // 读写: server.go, dashboard.go
	scratchH     *ScratchHandler      // 读写: dashboard.go (ctor only in server.go)
	memoryH      *MemoryHandler       // 读写: dashboard.go (ctor only in server.go)
	agentEventsH *AgentEventsHandlers // 读写: server.go, dashboard.go

	// ── Phase 5: → NewHub Options ──────────────────────
	dedup           *platform.Dedup              // 读写: server.go (ctor only)
	sessionGuard    *session.Guard               // 读写: server.go, dashboard.go
	msgQueue        *dispatch.MessageQueue       // 读写: server.go, dashboard.go (R242-GO-10: → wshub.MessageEnqueuer interface)
	agents          map[string]session.AgentOpts // 读写: server.go, dashboard.go, dashboard_session.go
	agentCommands   map[string]string            // 读写: server.go, dashboard.go
	dashboardToken  string                       // 读写: server.go, dashboard.go, dashboard_auth.go
	allowedRoot     string                       // 读写: server.go, dashboard.go (also Hub.allowedRoot — merge in Phase 4)
	noOutputTimeout time.Duration                // 读写: server.go (timeout error messages)
	totalTimeout    time.Duration                // 读写: server.go

	// ── Phase 5: → dashboard/* sub-packages ────────────
	claudeDir      string               // 读写: server.go, takeover.go, discovery_cache.go, dashboard_cron_transcript.go, dashboard_discovered.go, dashboard_session.go
	workspaceName  string               // 读写: server.go (ctor only; copied into SessionHandlers/HealthHandler)
	discoveryCache *discoveryCache      // 读写: server.go (background-cached local discovery results)
	scratchPool    *session.ScratchPool // 读写: server.go, dashboard.go, wshub.go (ephemeral aside sessions for preview drawer)
	sysessionMgr   *sysession.Manager   // 读写: dashboard.go, dashboard_system.go (system-daemon Tick scheduling)

	// ── Phase 5: → server-internal reorg ───────────────
	debugMode bool                 // 读写: dashboard.go (gates /api/debug/pprof and /api/debug/vars; R244-SEC-P3-1)
	resolver  *session.KeyResolver // 读写: server.go, dashboard.go (session-key → opts derivation; → routes.go local)
	nodeCache *node.CacheManager   // 读写: server.go (background-cached remote node data; → server/nodecache.go)

	// ── Phase 5: → metrics package ─────────────────────
	watchdogNoOutputKills atomic.Int64 // 读写: server.go (exposed via /health and /api/sessions)
	watchdogTotalKills    atomic.Int64 // 读写: server.go

	// ── Phase 5: pending evaluation (delete after grep verifies no usage) ─
	platforms  map[string]platform.Platform // 读写: server.go (likely routes-registration-only)
	backendTag string                       // 读写: server.go (ctor only; copied into SessionHandlers; v0.4 §六.6 待评估 → dispatch.BackendTag())
	knownNodes map[string]string            // 读写: server.go (configured node IDs → display names; merge into nodes map)
}

// Sentinel errors returned by validateWorkspace. Handlers map these onto
// status codes + machine-readable reason tags; they intentionally carry no
// path detail so error messages never leak host filesystem layout to
// authenticated dashboard / IM clients (the slog Debug line in
// validateWorkspace is the operator-side diagnostic surface).
//
// Why sentinels instead of one generic string: the previous design returned
// the same "workspace is not a valid directory" for IsAbs / EvalSymlinks /
// Stat / prefix-mismatch failures, leaving the dashboard to render a single
// "无权限或参数越界" toast for four very different operator-actionable
// causes. Cron handlers in particular have to distinguish "path doesn't
// exist on this host" (operator typo) from "path outside allowedRoot"
// (real boundary violation) so users can self-correct.
var (
	ErrWorkspaceInvalid     = errors.New("workspace not a valid path")
	ErrWorkspaceNotExist    = errors.New("workspace path does not exist")
	ErrWorkspaceNotDir      = errors.New("workspace path is not a directory")
	ErrWorkspaceOutsideRoot = errors.New("workspace outside allowed root")
)

// validateWorkspace checks that workspace is an existing directory within allowedRoot.
// Returns the cleaned, symlink-resolved path or one of the Err* sentinels above.
//
// Ordering: EvalSymlinks is performed first so the root-prefix check sees the
// canonical resolved path; only then do we Stat the resolved form. This
// collapses the TOCTOU window where a symlink swap between an initial Stat
// and a later EvalSymlinks could cause the two calls to observe different
// filesystem entries.
//
// Symmetry with cron.workDirUnderRoot: both wsPath AND allowedRoot are
// resolved via EvalSymlinks before the prefix check. Without this, a host
// where allowedRoot itself contains a symlinked component (e.g. `/home →
// /var/home` on some distros, Docker bind-mounts, AMI-customized layouts)
// would always fail the prefix check because resolved wsPath lands under
// the canonical path while allowedRoot stays in the un-resolved form.
// The resolved-root fallback chain (live EvalSymlinks → cached resolved →
// raw) mirrors internal/cron/scheduler.go workDirUnderRoot:443.
//
// Errors are sentinels — the resolved path and underlying os.PathError are
// NOT included so a dashboard or IM user cannot enumerate host filesystem
// layout via crafted workspace queries. Diagnostic detail goes to slog.Debug.
func validateWorkspace(workspace, allowedRoot string) (string, error) {
	if workspace == "" {
		return "", ErrWorkspaceInvalid
	}
	// Explicit absolute-path contract: filepath.Clean preserves relative input
	// verbatim, and when allowedRoot is absolute the HasPrefix check below
	// will always fail for a relative workspace — correct today but implicit.
	// Reject upfront so a future relative allowedRoot cannot silently admit
	// `../etc/passwd` style traversal.
	if !filepath.IsAbs(workspace) {
		return "", ErrWorkspaceInvalid
	}
	wsPath := filepath.Clean(workspace)
	resolved, err := filepath.EvalSymlinks(wsPath)
	if err != nil {
		// *os.PathError echoes the same path back through err.Error() which
		// lands in debug logs twice. Reduce to a structural kind so operators
		// can still distinguish "not exist" from "permission denied" without
		// a duplicate path column.
		slog.Debug("validateWorkspace: EvalSymlinks failed",
			"path", wsPath, "reason", pathErrReason(err))
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrWorkspaceNotExist
		}
		return "", ErrWorkspaceInvalid
	}
	wsPath = resolved
	info, err := os.Stat(wsPath)
	if err != nil {
		slog.Debug("validateWorkspace: Stat failed",
			"path", wsPath, "reason", pathErrReason(err))
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrWorkspaceNotExist
		}
		return "", ErrWorkspaceInvalid
	}
	if !info.IsDir() {
		slog.Debug("validateWorkspace: Stat failed",
			"path", wsPath, "reason", "not_a_directory")
		return "", ErrWorkspaceNotDir
	}
	if allowedRoot != "" {
		// Resolve allowedRoot the same way wsPath was resolved above so a
		// symlinked root component (e.g. `/home → /var/home`) doesn't cause
		// every call to fail the prefix check. EvalSymlinks failure on root
		// falls back to the raw path — matches cron.workDirUnderRoot.
		rootResolved, err := filepath.EvalSymlinks(allowedRoot)
		if err != nil {
			slog.Debug("validateWorkspace: allowedRoot EvalSymlinks failed; falling back to raw",
				"root", allowedRoot, "reason", pathErrReason(err))
			rootResolved = allowedRoot
		}
		if wsPath != rootResolved &&
			!strings.HasPrefix(wsPath, rootResolved+string(filepath.Separator)) {
			return "", ErrWorkspaceOutsideRoot
		}
	}
	return wsPath, nil
}

// classifyWorkspaceErr maps a validateWorkspace sentinel onto (HTTP status,
// public message). Centralising the mapping here keeps every handler
// (cron, send, takeover) returning consistent status codes and reason
// tags. The reason tag is short ASCII so the dashboard's localizeAPIError
// can show it verbatim in the tail without leaking host filesystem paths.
//
// Why two channels (status code + tag string): clients need the status
// code for retry/redirect logic and the tag to render an actionable
// message. "invalid work_dir" alone forced operators to read server logs
// to know whether they typo'd the path, picked a non-existent project,
// or hit the allowedRoot boundary.
func classifyWorkspaceErr(err error) (int, string) {
	switch {
	case errors.Is(err, ErrWorkspaceOutsideRoot):
		return http.StatusForbidden, "work_dir outside allowed root"
	case errors.Is(err, ErrWorkspaceNotExist):
		return http.StatusBadRequest, "work_dir does not exist"
	case errors.Is(err, ErrWorkspaceNotDir):
		return http.StatusBadRequest, "work_dir is not a directory"
	case errors.Is(err, ErrWorkspaceInvalid):
		return http.StatusBadRequest, "work_dir is not a valid path"
	default:
		// Defensive: unknown error type → conservative 403 generic.
		// Should never happen because validateWorkspace only returns the
		// sentinels above.
		return http.StatusForbidden, "invalid work_dir"
	}
}

// validateRemoteWorkspace is the primary-side syntactic check applied to a
// workspace string that will be forwarded to a remote reverse node via the
// RPC "send" method. The primary cannot Stat the remote filesystem, but it
// can and should reject obviously unsafe inputs — absolute path shape, no
// NUL, no control bytes, bounded length, no traversal markers — before
// relaying. Without this guard, an authenticated dashboard user could
// submit `../../../etc` as a workspace to a node whose defaultWorkspace is
// empty and have the remote connector happily bind it. The remote node
// runs its own EvalSymlinks check, but that check uses the node's own
// defaults; sharing the primary's allowedRoot across nodes is not always
// possible (nodes may have different filesystem layouts). R61-SEC-2.
func validateRemoteWorkspace(workspace string) error {
	// Delegate to the canonical session-layer validator so the two trust
	// boundaries (primary HTTP / RPC) cannot drift. session.ValidateRemote-
	// WorkspacePath additionally does a utf8.ValidString sweep which the
	// previous inline byte-level scan here missed — an attacker could
	// submit a non-UTF8 byte sequence like 0xFF 0xFE that passes the
	// `< 0x20` check yet corrupts slog TextHandler output downstream.
	return session.ValidateRemoteWorkspacePath(workspace)
}

// pathErrReason returns a short, path-free tag describing a filesystem error
// so debug logs do not echo the workspace path twice via *os.PathError.
func pathErrReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return "not_exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	default:
		return "fs_error"
	}
}

// loadOrCreateCookieSecret reads a 32-byte secret from stateDir/cookie_secret,
// creating it with crypto/rand if absent. Falls back to a fresh ephemeral secret
// if the file cannot be read or written (e.g. no stateDir configured).
func loadOrCreateCookieSecret(stateDir string) []byte {
	if stateDir != "" {
		// Defence in depth: the symlink check below pins cookie_secret
		// itself, but a local attacker who can repoint stateDir (e.g.
		// stateDir → /tmp/pwn/ because the parent is world-writable)
		// bypasses that by placing a well-formed cookie_secret inside
		// their own directory. Lstat'ing stateDir first makes that
		// class of attack visible — a symlink'd stateDir gets flagged
		// and the secret is regenerated (ephemeral fallback) instead
		// of silently trusting whatever the target directory serves.
		if fi, err := os.Lstat(stateDir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			slog.Error("cookie_secret regenerated because stateDir is a symlink",
				"state_dir", stateDir, "reason", "statedir_symlink")
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				panic("crypto/rand unavailable: " + err.Error())
			}
			return b
		}
		path := filepath.Join(stateDir, "cookie_secret")
		// R188-SEC-L4: use os.Lstat so a symlink attack (e.g. cookie_secret →
		// /etc/some-readable-file) is detected instead of silently validated
		// against the target's mode. A local attacker who can write stateDir
		// would otherwise trick the check into passing against an arbitrary
		// file and leak its contents via the cookie secret ABI, or trigger
		// rotation loops that invalidate all sessions.
		if fi, err := os.Lstat(path); err == nil {
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				slog.Error("cookie_secret regenerated because file is a symlink",
					"path", path, "reason", "symlink")
			case fi.Mode().Perm() != 0600:
				// Log at Error with an explicit reason so monitoring can
				// distinguish "unsafe perms forced rotation" from first-run
				// regeneration. All existing browser sessions will be
				// invalidated — operator should know why.
				slog.Error("cookie_secret regenerated due to unsafe permissions",
					"path", path, "mode", fi.Mode().Perm(), "reason", "unsafe_permissions")
			default:
				if data, err := os.ReadFile(path); err == nil && len(data) == 32 {
					return data
				}
			}
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic("crypto/rand unavailable: " + err.Error())
		}
		// R236-SEC-10: persistence is best-effort, but failure must surface
		// at Error level (not Warn). When the secret cannot be persisted the
		// process keeps running with an in-memory secret — the side effect
		// is that every restart silently invalidates every browser session,
		// which operators will mistake for a token expiry bug rather than a
		// disk / permissions misconfiguration. Error-level lines + an
		// explicit reason make the failure mode greppable in logs.
		if err := os.MkdirAll(stateDir, 0700); err != nil {
			slog.Error("cookie_secret stateDir mkdir failed; session secret is ephemeral, all sessions will be invalidated on restart",
				"state_dir", stateDir, "err", err, "reason", "mkdir_failed")
		} else {
			// Write atomically (tmp + rename) so a concurrent reader never
			// sees a partial secret during rotation. os.WriteFile opens with
			// O_TRUNC and the crypto/rand bytes land in small chunks — a
			// parallel open+read could pick up N bytes of zeros if we were
			// mid-Write. WriteFileAtomic also fsyncs the file + parent dir.
			if err := osutil.WriteFileAtomic(path, b, 0600); err != nil {
				slog.Error("cookie_secret atomic write failed; session secret is ephemeral, all sessions will be invalidated on restart",
					"path", path, "err", err, "reason", "write_failed")
			}
		}
		return b
	}
	// No stateDir: ephemeral secret (sessions lost on restart)
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return b
}

// replyTagForBackend resolves a backend ID (e.g. "claude" / "kiro") to the
// short tag the dispatch layer appends to outbound IM replies ("cc" /
// "kiro"). Reads from the cli/backend Profile registry; unknown ids return
// "" so dispatch skips the footer rather than emitting garbled output.
//
// Empty backend ID resolves to "cc" so legacy single-backend deployments
// without the Backend field on stored sessions keep their historical
// "[cc]" footer. docs/rfc/multi-backend.md §7.
//
// Registry-not-ready path: production wires backend.RegisterDefaults() in
// cmd/naozhi/main.go before any server constructs. Tests that build a
// Server without that call would see backend.Get return false and lose the
// tag — replyTagForBackendOnce ensures a one-shot lazy registration so tests
// remain green without each having to wire the registry by hand.
func replyTagForBackend(id string) string {
	replyTagForBackendOnce.Do(func() {
		if len(backend.All()) == 0 {
			backend.RegisterDefaults()
		}
	})
	if id == "" {
		id = "claude"
	}
	if p, ok := backend.Get(id); ok {
		return p.DefaultTag
	}
	return ""
}

var replyTagForBackendOnce sync.Once

// New creates a new Server.
// ServerOptions holds optional configuration for a Server.
// All fields have zero-value defaults (empty string, nil, zero duration = disabled/unset).
type ServerOptions struct {
	WorkspaceID       string
	WorkspaceName     string
	AllowedRoot       string // restricts /cd to paths under this root
	StateDir          string // directory for persistent state (cookie_secret, etc.)
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
	Transcriber       transcribe.Service
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

	// PublicTmpEnabled opts the __public_tmp__ pseudo-project in (R237-SEC-5,
	// #646). When false (default) requests for that pseudo-project fall
	// through to the regular "project not found" surface — closes the
	// "any authed dashboard user can read /tmp" gap on multi-user
	// deployments. Single-operator dashboards (the typical naozhi use)
	// flip it on via `server.public_tmp_enabled: true` in config.yaml so
	// chat-mentioned /tmp/... paths still resolve without first
	// registering /tmp as a real project.
	PublicTmpEnabled bool

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
}

// NewWithOptions constructs a Server from a single ServerOptions value.
// Prefer this constructor for new call sites — it reads like a config
// literal and tolerates new fields being added without signature churn.
// The legacy New() wrapper still exists for backward compatibility.
//
// Required: opts.Router must be non-nil. opts.Addr must be set for the
// listener to bind. Other fields tolerate zero values.
func NewWithOptions(opts ServerOptions) *Server {
	return buildServer(opts)
}

// New is the legacy positional-args constructor, retained so existing
// call sites (especially tests) do not need mechanical updates. It
// stuffs the positional args into ServerOptions (overriding any matching
// fields the caller may have also set in opts) and delegates to
// NewWithOptions. New callers should use NewWithOptions directly.
//
// Deprecated: use NewWithOptions. Production (cmd/naozhi/main.go) already
// calls NewWithOptions; this signature is kept to avoid churning ~20 test
// call sites at once — they can migrate in-place at any future touch.
// Gopls / staticcheck will flag new positional-style call sites so the
// migration path stays discoverable.
//
// Removal condition (R214-CODE-4 / R224-CR-5): delete this wrapper once
// every *_test.go in this package and its consumers calls NewWithOptions
// directly. Track via `git grep -l "server.New("` returning zero hits.
// New positional-style call sites should NOT be added — start with
// NewWithOptions and let this function shrink to test-only legacy.
func New(addr string, router *session.Router, platforms map[string]platform.Platform, agents map[string]session.AgentOpts, agentCommands map[string]string, scheduler *cron.Scheduler, backend string, opts ServerOptions) *Server {
	opts.Addr = addr
	opts.Router = router
	opts.Platforms = platforms
	opts.Agents = agents
	opts.AgentCommands = agentCommands
	opts.Scheduler = scheduler
	opts.Backend = backend
	return NewWithOptions(opts)
}

// buildServer is the shared construction path used by both New and
// NewWithOptions. Kept private so the two public entry points are the
// only way to create a *Server, and their contracts can evolve
// independently without leaking internal assembly details.
func buildServer(opts ServerOptions) *Server {
	addr := opts.Addr
	router := opts.Router
	platforms := opts.Platforms
	agents := opts.Agents
	agentCommands := opts.AgentCommands
	scheduler := opts.Scheduler
	defaultBackend := opts.Backend
	// defaultTag is the fallback ReplyFooter tag for sessions whose
	// Backend() is empty (legacy stores predating the multi-backend Backend
	// field). docs/rfc/multi-backend.md §7.
	defaultTag := replyTagForBackend(defaultBackend)
	// tag is retained as the legacy server-global value (backendTag field +
	// SessionStats.Backend). Per-session ReplyFooterFn (wired below) reads
	// session.Backend() at IM-reply time so a kiro session in a claude-default
	// deployment gets [kiro] correctly.
	tag := defaultTag
	// R222-ARCH-9 / #724: env probe goes through the shared helper so the
	// "where is ~/.claude" decision lives in one place (claude_paths.go).
	// resolveClaudeDir returns "" when UserHomeDir fails, matching the
	// previous inline shape exactly — downstream sites already nil-check
	// claudeDir before joining or reading.
	claudeDir := resolveClaudeDir()

	nodes := opts.Nodes
	if nodes == nil {
		nodes = make(map[string]node.Conn)
	}
	knownNodes := make(map[string]string)
	for id, nc := range nodes {
		knownNodes[id] = nc.DisplayName()
	}

	// allowed_root is the one directory-traversal guard for dashboard /cd,
	// cron WorkDir, and handleTakeover CWD. Empty means "no restriction",
	// which is the legitimate single-user default but a real risk in
	// multi-user deployments. Surface it once at startup so operators can
	// audit their config rather than discovering the looseness via incident.
	//
	// R226-SEC-6: when both (a) a dashboard_token is configured (multi-user
	// intent) AND (b) the bind address is non-loopback (network-reachable),
	// raise the visibility from a single Warn line to two with explicit
	// "high severity" wording. Upgrading to fatal is deferred — existing
	// deployments rely on the warn-only contract and a hard-fail here
	// would break upgrades; `naozhi doctor` is the right place for the
	// fatal escalation. This pair-warn at least guarantees operators see
	// the risk before an incident, mapping onto the TODO's "升级 warn 严重度"
	// ask while preserving boot-compat.
	//
	// R237-SEC-9 / #658: the multi-user-intent + network-reachable branch
	// upgrades from Warn to Error. The single-user/loopback branch stays
	// Warn because that's the legitimate dev-laptop default. Error level
	// (a) routes to stderr in slog default text handler, (b) shows up
	// distinctly under journald PRIORITY filtering, and (c) trips alerting
	// pipelines that ignore Warn. The boot itself is intentionally not
	// failed — `naozhi doctor` remains the right place for hard-fail
	// because operators can run it standalone before exposing the listener
	// without burning a service-restart cycle on an upgrade.
	if opts.AllowedRoot == "" {
		slog.Warn("server.allowed_root is unset; dashboard /cd, cron WorkDir, and takeover CWD accept any absolute path — set allowed_root in config.yaml to restrict")
		if opts.DashboardToken != "" && isPlaintextPublicAddr(opts.Addr) {
			slog.Error("allowed_root unset on a token-protected, network-reachable dashboard — any authenticated user can set cron WorkDir to /etc or other system paths and let the CLI write there. Set server.allowed_root before exposing this listener; `naozhi doctor` will hard-fail this configuration.",
				"addr", opts.Addr,
			)
		}
	}

	cookieSecret := loadOrCreateCookieSecret(opts.StateDir)
	// R247-SEC-17: cookieGen is mixed into the auth-cookie HMAC alongside
	// the dashboard token so every restart produces a fresh MAC even when
	// stateDir is shared (the common operator setup). nanoseconds are
	// unique within the process and cheap to produce — we don't need
	// crypto-grade entropy because cookieSecret already supplies that;
	// cookieGen exists only to break MAC equivalence across (re)starts
	// and future hot-reloads. Future rotation handler can bump this field
	// at runtime to invalidate every outstanding cookie atomically.
	cookieGen := strconv.FormatInt(time.Now().UnixNano(), 10)

	// Construct KeyResolver once and share across dispatcher (wired in
	// Start), hub, and ProjectHandlers. project.NewDataSource returns
	// untyped nil when projectMgr is nil so the Resolver correctly
	// short-circuits the project-binding lookup in that mode.
	// docs/rfc/key-resolver.md Phase 4.
	resolver := session.NewKeyResolver(agents, project.NewDataSource(opts.ProjectManager))

	s := &Server{
		addr:         addr,
		mux:          http.NewServeMux(),
		platforms:    platforms,
		router:       router,
		dedup:        platform.NewDedup(defaultDedupCapacity),
		sessionGuard: session.NewGuard(),
		msgQueue: dispatch.NewMessageQueueWithMode(
			opts.QueueMaxDepth,
			opts.QueueCollectDelay,
			dispatch.ParseQueueMode(opts.QueueMode),
		),
		startedAt:       time.Now(),
		agents:          agents,
		agentCommands:   agentCommands,
		scheduler:       scheduler,
		backendTag:      tag,
		claudeDir:       claudeDir,
		workspaceName:   opts.WorkspaceName,
		allowedRoot:     opts.AllowedRoot,
		noOutputTimeout: opts.NoOutputTimeout,
		totalTimeout:    opts.TotalTimeout,
		dashboardToken:  opts.DashboardToken,
		debugMode:       opts.DebugMode,
		onReady:         opts.OnReady,
		projectMgr:      opts.ProjectManager,
		resolver:        resolver,
		nodes:           nodes,
		knownNodes:      knownNodes,
		sysessionMgr:    opts.SysessionManager,

		// Extracted handler groups (literals factored to build_handlers.go;
		// #738 / R246-CR-004). Helper docstrings carry the limiter rationale
		// that previously lived inline; buildServer keeps initialization
		// order visible while shedding ~40 LOC of struct literals.
		auth:        buildAuthHandlers(opts, cookieSecret, cookieGen),
		cronH:       buildCronHandlers(opts, claudeDir),
		transcribeH: buildTranscribeHandler(opts),
	}

	// Q1: the router's terminal-removal hook (Router.Reset/Remove; LRU
	// evictOldest deliberately does NOT fire it) is wired below — once
	// AFTER s.sessionH is constructed — so the single registration fans
	// out atomically to both cleanup riders:
	//
	//   1. msgQueue.Cleanup so the per-session FIFO map entry is truly
	//      deleted when the user resets or removes a session (/new,
	//      dashboard delete). Without this the entry is retained forever
	//      for gen-monotonicity — fine under LRU eviction (the session
	//      might return) but a slow leak when the key is never reused.
	//   2. sessionH.InvalidateHistoryCache so the history popover sees
	//      the just-retired session within one /api/sessions poll
	//      instead of being hidden by the 120s TTL.
	//
	// R238-CR-3: an earlier draft registered msgQueue.Cleanup here first
	// and overwrote the hook with the full fanout after sessionH was
	// constructed. WarmHistoryCache (a background goroutine) could
	// observe a Reset in that overwrite window with InvalidateHistoryCache
	// not yet wired. Skipping the half-wired registration removes the
	// race entirely; until the replay below, msgQueue.Cleanup is reachable
	// only via direct calls (router has no triggers active before the
	// first session/cron is started).

	// Construct the retired-store eagerly so the SessionHandlers below
	// can hold a non-nil pointer; the actual file load is best-effort
	// and a parse error here just means the store starts empty. The
	// store is only persisted when StateDir is configured — in-memory
	// only otherwise (tests, ephemeral deployments).
	retiredStore, retiredErr := buildRetiredStoreWithErr(opts.StateDir)
	if retiredErr != nil {
		slog.Warn("retired store load failed (degrades to last_active sort)", "err", retiredErr)
	}

	s.nodeAccess = newNodeAccessor(&s.nodesMu, s.nodes, s.knownNodes)

	hubBroadcast := func() {
		if s.hub != nil {
			s.hub.BroadcastSessionsUpdate()
		}
	}

	s.nodeCache = node.NewCacheManager(
		func() map[string]node.Conn {
			return s.nodeAccess.NodesSnapshot()
		},
		hubBroadcast,
	)

	s.discoveryCache = newDiscoveryCache(claudeDir, s.router.ManagedExcludeSets, opts.ProjectManager)

	// Wire extracted handler groups that depend on nodeAccess/nodeCache
	// (literals live in build_handlers.go; #738).
	s.discoveryH = buildDiscoveryHandlers(opts, claudeDir, s.discoveryCache, s.nodeAccess, s.nodeCache, hubBroadcast)
	// R247-ARCH-15 (#650): no closure here — ProjectHandlers stores
	// baseCtx as a plain field that registerDashboard wires via
	// SetBaseContext once `s.hub` exists. The two-phase construction
	// is unchanged (Hub still doesn't exist at this point); only the
	// DI shape moved from a captured closure to a direct field assign.
	s.projectH = buildProjectHandlers(opts, resolver, s.nodeAccess, s.nodeCache)
	agentIDs := agentIDList(agents)
	s.sessionH = &SessionHandlers{
		router:        router,
		projectMgr:    opts.ProjectManager,
		scheduler:     scheduler,
		cronSessions:  scheduler, // *cron.Scheduler implements cronSessionLister via KnownSessionIDs
		sysWorkDir:    opts.SysWorkDir,
		claudeDir:     claudeDir,
		allowedRoot:   opts.AllowedRoot,
		agents:        agents,
		agentIDs:      agentIDs,
		nodeAccess:    s.nodeAccess,
		nodeCache:     s.nodeCache,
		startedAt:     s.startedAt,
		backendTag:    tag,
		workspaceID:   opts.WorkspaceID,
		workspaceName: opts.WorkspaceName,
		versionTag:    opts.Version,
		watchdogNoOut: &s.watchdogNoOutputKills,
		watchdogTotal: &s.watchdogTotalKills,
		retiredStore:  retiredStore,
	}
	s.sessionH.initStaticStats()
	s.sessionH.WarmHistoryCache()
	// Replay SetOnKeyRetired now that sessionH exists, fanning out to both
	// msgQueue.Cleanup and InvalidateHistoryCache. See the rationale at the
	// initial SetOnKeyRetired call earlier in New().
	{
		msgCleanup := s.msgQueue.Cleanup
		sessionH := s.sessionH
		router.SetOnKeyRetired(func(key string) {
			msgCleanup(key)
			sessionH.InvalidateHistoryCache()
		})
	}

	// Wire the session-retired hook independently of msgQueue.Cleanup
	// (which uses SetOnKeyRetired). Capturing s.sessionH in the closure
	// keeps the call zero-allocation in the steady state — atomic load
	// + 1 method call + 1 mutex op inside the store.
	router.SetOnSessionRetired(func(_ string, sessionID string) {
		s.sessionH.RecordRetired(sessionID)
	})
	s.agentEventsH = &AgentEventsHandlers{
		router:     router,
		nodeAccess: s.nodeAccess,
	}

	// Scratch pool (ephemeral aside sessions). Bound to the same router so
	// scratches flow through the standard spawn/send/event path as managed
	// sessions; the saveStore/handleList filters on the "scratch:" prefix
	// keep them off the sidebar and out of sessions.json. The sweeper is
	// started later in registerDashboard so an early New() failure does not
	// leak the ticker goroutine.
	s.scratchPool = session.NewScratchPool(router, session.DefaultScratchMax, session.DefaultScratchTTL)
	// Thread StartupCtx into the --version probe so SIGTERM during
	// startup aborts promptly (R55-QUAL-004). Nil ctx falls back to
	// NewCLIBackendsHandler's Background-derived path via the delegating
	// public ctor (keeps test/headless callers working).
	if opts.StartupCtx != nil {
		s.cliH = NewCLIBackendsHandlerCtx(opts.StartupCtx, router)
	} else {
		s.cliH = NewCLIBackendsHandler(router)
	}
	platNames := platformNameSet(platforms)
	s.healthH = &HealthHandler{
		router:             router,
		auth:               s.auth,
		startedAt:          s.startedAt,
		workspaceID:        opts.WorkspaceID,
		workspaceName:      opts.WorkspaceName,
		version:            opts.Version,
		noOutputTimeout:    opts.NoOutputTimeout,
		totalTimeout:       opts.TotalTimeout,
		noOutputTimeoutStr: opts.NoOutputTimeout.String(),
		totalTimeoutStr:    opts.TotalTimeout.String(),
		watchdogNoOut:      &s.watchdogNoOutputKills,
		watchdogTotal:      &s.watchdogTotalKills,
		nodeAccess:         s.nodeAccess,
		platforms:          platNames,
		hubDropped: func() int64 {
			if s.hub == nil {
				return 0
			}
			return s.hub.DroppedMessages()
		},
	}
	// sendH is wired after registerDashboard creates hub

	if opts.ReverseNodeServer != nil {
		s.reverseNodeServer = opts.ReverseNodeServer
		for id, displayName := range opts.ReverseNodeServer.AllNodes() {
			s.knownNodes[id] = displayName
		}
		opts.ReverseNodeServer.OnRegister = func(id string, rc *node.ReverseConn) {
			s.nodesMu.Lock()
			s.nodes[id] = rc
			s.nodesMu.Unlock()
			go s.nodeCache.RefreshFor(id) // RefreshFor calls onChange → BroadcastSessionsUpdate
		}
		opts.ReverseNodeServer.OnDeregister = func(id string) {
			s.nodesMu.Lock()
			delete(s.nodes, id)
			s.nodesMu.Unlock()
			s.nodeCache.PurgeNode(id)
			if s.hub != nil {
				s.hub.PurgeNodeSubscriptions(id)
				s.hub.BroadcastSessionsUpdate()
			}
		}
	}

	// R230C-SEC-11: when a scheduler is wired (cron endpoints active),
	// runsLimiter MUST be non-nil. The handlers nil-guard the limiter to
	// support test bridging that constructs a partial CronHandlers, but a
	// future server.New refactor that forgets to wire runsLimiter would
	// silently downgrade to unlimited rate. Fail-fast at construction so
	// the regression surfaces during boot rather than under attack.
	//
	// R242-CR-3: same guard for listLimiter — handleList is the cron
	// dashboard's heartbeat and the most attractive enumeration target
	// of the cron HTTP surface, so silent unlimited-rate downgrade is
	// unacceptable.
	if s.scheduler != nil && s.cronH != nil {
		if s.cronH.runsLimiter == nil {
			panic("server: runsLimiter must be non-nil when scheduler is wired")
		}
		if s.cronH.listLimiter == nil {
			panic("server: listLimiter must be non-nil when scheduler is wired")
		}
		// [R247-SEC-2 / R247-SEC-3] writeLimiter gates trigger + preview;
		// silent unlimited-rate downgrade would expose CLI-spawn /
		// IM-notify amplification, so fail-fast at construction.
		if s.cronH.writeLimiter == nil {
			panic("server: writeLimiter must be non-nil when scheduler is wired")
		}
	}

	return s
}

// Start registers routes and begins serving.
func (s *Server) Start(ctx context.Context) error {
	// Resolver is constructed in buildServer and reused across the
	// dispatch / hub / project-api surfaces. docs/rfc/key-resolver.md
	// Phase 4.
	d, err := dispatch.NewDispatcher(dispatch.DispatcherConfig{
		Router:                s.router,
		Platforms:             s.platforms,
		Agents:                s.agents,
		AgentCommands:         s.agentCommands,
		Scheduler:             s.scheduler,
		ProjectMgr:            s.projectMgr,
		Resolver:              s.resolver,
		Guard:                 s.sessionGuard,
		Queue:                 s.msgQueue,
		Dedup:                 s.dedup,
		AllowedRoot:           s.allowedRoot,
		ClaudeDir:             s.claudeDir,
		Capabilities:          serverCaps{s: s},
		NoOutputTimeout:       s.noOutputTimeout,
		TotalTimeout:          s.totalTimeout,
		WatchdogNoOutputKills: &s.watchdogNoOutputKills,
		WatchdogTotalKills:    &s.watchdogTotalKills,
	})
	if err != nil {
		// R250-ARCH-12: missing Send wireup is a boot-time configuration
		// fault. Surface it through Server.Start's existing error return
		// path so systemd logs the cause and the unit fails fast instead
		// of crashing on first user message.
		return fmt.Errorf("dispatch wireup: %w", err)
	}
	// Expose dispatcher counters via /health. The handler is constructed
	// earlier in New() without a dispatcher reference, so we wire the
	// closure here once the dispatcher exists.
	if s.healthH != nil {
		s.healthH.dispatcherMetrics = d.Metrics
	}
	handler := d.BuildHandler()

	var startedPlatforms []platform.RunnablePlatform
	for _, p := range s.platforms {
		p.RegisterRoutes(s.mux, handler)
		slog.Info("platform registered", "name", p.Name())

		if rp, ok := p.(platform.RunnablePlatform); ok {
			if err := rp.Start(handler); err != nil {
				// Stop already-started platforms to avoid connection leaks.
				// Log individual stop failures; a silent rollback could mask
				// a dangling websocket that holds the process open past the
				// fatal startup error we're about to return.
				for _, sp := range startedPlatforms {
					if stopErr := sp.Stop(); stopErr != nil {
						slog.Warn("platform rollback stop failed",
							"name", sp.Name(), "err", stopErr)
					}
				}
				return fmt.Errorf("start platform %s: %w", p.Name(), err)
			}
			startedPlatforms = append(startedPlatforms, rp)
		}
	}

	s.mux.HandleFunc("GET /health", s.healthH.handleHealth)
	s.appCtx = ctx
	s.discoveryH.appCtx = ctx
	s.registerDashboard()
	s.nodeCache.StartLoop(ctx)
	s.discoveryCache.startLoop(ctx)
	s.startProjectScanLoop(ctx)
	// Warn if we're serving a token-protected dashboard over plaintext with no
	// trusted proxy in front — Bearer tokens and auth cookies would traverse
	// the wire in the clear, subject to passive sniffing on shared networks.
	// `trustedProxy=true` is the operator's explicit statement that TLS
	// termination happens upstream (ALB/CloudFront), in which case this
	// listener binding to plaintext loopback is fine.
	if s.dashboardToken != "" && !s.auth.trustedProxy && isPlaintextPublicAddr(s.addr) {
		slog.Warn(
			"dashboard token served over plaintext HTTP with no trusted proxy: "+
				"bearer tokens and session cookies may be sniffed. "+
				"Terminate TLS upstream and set server.trusted_proxy=true, "+
				"or bind to 127.0.0.1 for local-only access.",
			"addr", s.addr,
		)
	}
	// No-auth mode on a publicly reachable address is the biggest footgun the
	// operator can step into — every /api/* endpoint becomes world-reachable.
	// Decision logic extracted to shouldWarnNoTokenOpen for unit-test coverage;
	// see R60-SEC-006 / R70-SEC-M1 in the helper's docstring.
	if shouldWarnNoTokenOpen(s.dashboardToken, s.addr, s.auth.trustedProxy) {
		slog.Warn(noTokenOpenWarning,
			"addr", s.addr,
			"trusted_proxy", s.auth.trustedProxy,
		)
	} else if s.dashboardToken == "" {
		// Loopback + no token is the "local dev" happy path, but if a systemd
		// unit or orchestration layer accidentally clears the token the
		// operator gets no signal that auth is off. Log once at startup so
		// journalctl shows the state regardless of reachability. R23-SEC-M5.
		slog.Warn("dashboard token not configured; all API callers accepted without authentication",
			"addr", s.addr,
		)
	}
	// /ws-node reverse-node channel sends node tokens and session payloads in
	// plaintext when the primary binds to a public HTTP address with no TLS
	// terminator upstream. Passive sniffers on the path can lift the token and
	// impersonate the remote node. Mirror the dashboard token warning so the
	// operator sees the same shape of signal in the startup journal. R176-SEC-MED.
	if shouldWarnReverseNodePlaintext(s.reverseNodeServer != nil, s.auth.trustedProxy, s.addr) {
		slog.Warn(reverseNodePlaintextWarning,
			"addr", s.addr,
		)
	}
	// R238-SEC-15 (#848): when trustedProxy=true, every per-IP rate limiter,
	// per-IP audit slog field, and same-origin gate decision flows from the
	// last X-Forwarded-For hop. If the upstream proxy does NOT strip
	// client-supplied XFF headers before re-appending its own (or honours
	// arbitrary-depth XFF without a hop-count limit), an attacker can spoof
	// the source IP by sending `X-Forwarded-For: <victim>, <attacker>` —
	// every per-IP gate then attributes the request to the victim's bucket.
	// The fix MUST happen at the proxy (e.g. ALB/CloudFront drop-and-replace,
	// nginx `real_ip_recursive on` with a trusted-proxy allowlist) — naozhi
	// honouring the last XFF hop is by design once trustedProxy is set.
	// Surface a one-shot info-level reminder at startup so an operator
	// who flipped trustedProxy=true on a misconfigured upstream sees the
	// requirement in the boot journal rather than discovering it via a
	// rate-limiter bypass weeks later. Info-level (not Warn) because the
	// configuration itself is legitimate — the warning is about the
	// upstream contract, which we cannot verify from inside the process.
	if s.auth.trustedProxy {
		slog.Info(trustedProxyXFFReminder, "addr", s.addr)
	}
	slog.Info("server starting", "addr", s.addr)

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}

	srv := &http.Server{
		Handler:           gzipMiddleware(s.mux),
		ReadHeaderTimeout: 5 * time.Second, // Slowloris defense
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Cap header bytes well below the default 1 MB so an unauthenticated
		// client can't force us to buffer megabyte-sized headers before
		// ReadHeaderTimeout fires. 64 KB is generous for legitimate cookies
		// plus a modest number of X-Forwarded-* headers.
		MaxHeaderBytes: 64 * 1024,
	}

	// Notify caller that the listener is bound and ready to accept connections.
	if s.onReady != nil {
		s.onReady()
	}

	// Periodic flush + prune for the retired-store. 30 s is a balance
	// between "lose at most ~30 s of marks on a SIGKILL" and keeping
	// fsync churn modest under burst close-many sessions UX. Prune
	// drops entries older than 14 days (= 2× the 7-day history window)
	// so the cap pressure described in NewRetiredStoreWithCap rarely
	// matters on a normal operator. Stops via ctx.Done in the shutdown
	// goroutine below.
	if s.sessionH != nil && s.sessionH.retiredStore != nil {
		go s.runRetiredStoreFlusher(ctx)
	}

	shutdownComplete := make(chan struct{})
	go func() {
		<-ctx.Done()
		slog.Info("shutting down server")

		// Shutdown WebSocket hub
		if s.hub != nil {
			s.hub.Shutdown()
		}

		// Stop the scratch-pool sweeper so its ticker goroutine exits before
		// the listener teardown completes. Stop is idempotent and drains in
		// under a second in practice.
		if s.scratchPool != nil {
			s.scratchPool.Stop()
		}

		// Drain any in-flight WarmHistoryCache goroutine before tearing down
		// the rest of the server. Without this wait the background FS scan
		// could write h.historyCache after claudeDir-dependent state is gone.
		// R64-GO-M1.
		if s.sessionH != nil {
			s.sessionH.WaitWarmHistory()
			// Flush the retired-store one final time so the most recent
			// retirement event survives a restart. The periodic flusher
			// below writes every 30s; a clean shutdown that landed
			// between ticks would otherwise lose a few entries.
			s.sessionH.FlushRetiredStore()
		}

		// Wait for the initial discovery refresh goroutine (R218-GO-1).
		// ctx is already cancelled above; Wait ensures the goroutine has
		// exited before projectMgr-dependent state is torn down.
		if s.discoveryCache != nil {
			s.discoveryCache.Wait()
		}

		// Stop RunnablePlatforms (e.g. WebSocket connections)
		for _, p := range s.platforms {
			if rp, ok := p.(platform.RunnablePlatform); ok {
				if err := rp.Stop(); err != nil {
					slog.Error("stop platform", "name", p.Name(), "err", err)
				}
			}
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), session.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "err", err)
		}
		close(shutdownComplete)
	}()

	err = srv.Serve(ln)
	// If ListenAndServe failed for a non-shutdown reason (e.g. port conflict),
	// return immediately instead of blocking — the shutdown goroutine is still
	// waiting on ctx.Done and shutdownComplete will never close.
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Wait for the shutdown goroutine to finish draining connections.
	select {
	case <-shutdownComplete:
	case <-ctx.Done():
		<-shutdownComplete
	}
	return err
}

// retiredStoreFlushInterval is how often runRetiredStoreFlusher writes
// the in-memory retired-store map to disk. 30 s loses at most a single
// burst of retirements on a hard kill while keeping fsync churn modest
// when an operator closes many sessions in a row.
const retiredStoreFlushInterval = 30 * time.Second

// retiredStorePruneInterval governs how often runRetiredStoreFlusher
// drops entries that fall outside the 7-day history window. A 14-day
// cutoff (= 2× the window) avoids racing the dashboard on entries that
// just dropped off the popover; the cap inside RetiredStore.Prune
// handles the pathological "operator closed thousands of sessions in
// the cutoff window" case.
const (
	retiredStorePruneInterval = 6 * time.Hour
	retiredStorePruneCutoff   = 14 * 24 * time.Hour
)

// runRetiredStoreFlusher writes the retired-store to disk every
// retiredStoreFlushInterval and prunes stale entries every
// retiredStorePruneInterval. Stops on ctx.Done; the shutdown goroutine
// invokes a final FlushRetiredStore so the most recent retirement event
// survives a clean shutdown.
func (s *Server) runRetiredStoreFlusher(ctx context.Context) {
	flushTicker := time.NewTicker(retiredStoreFlushInterval)
	defer flushTicker.Stop()
	pruneTicker := time.NewTicker(retiredStorePruneInterval)
	defer pruneTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flushTicker.C:
			s.sessionH.FlushRetiredStore()
		case <-pruneTicker.C:
			cutoffMs := time.Now().Add(-retiredStorePruneCutoff).UnixMilli()
			s.sessionH.retiredStore.Prune(cutoffMs)
		}
	}
}

// noTokenOpenWarning is the message logged when the API accepts any caller
// because dashboard_token is unset on a publicly reachable bind. Exposed as
// a package-level var (not a const literal in the caller) so tests can
// assert the exact text in journal/log output without re-typing it. The
// message intentionally enumerates the concrete risks so an operator
// scrolling a startup log has enough context to act without docs lookup.
const noTokenOpenWarning = "no dashboard_token configured on a non-loopback bind: " +
	"the ENTIRE dashboard API is open to any caller. " +
	"Anyone reaching this port can send messages to sessions, read workspace files under allowed_root, " +
	"alter cron schedules, and trigger transcription. Also: uploadOwner falls back to client IP, " +
	"so users sharing a NAT / LAN / egress gateway can see each other's inline uploads. " +
	"Either set server.dashboard_token, bind to 127.0.0.1 for single-user use, " +
	"or set server.trusted_proxy=true with an upstream that enforces access control."

// reverseNodePlaintextWarning is the message logged when /ws-node is exposed
// on a non-loopback plaintext HTTP bind with no trusted proxy in front. Named
// constant (not an inline literal) so tests can assert the exact journal text
// and a refactor that rewords one occurrence has a single source of truth to
// update. R176-SEC-MED.
const reverseNodePlaintextWarning = "reverse-node /ws-node endpoint served over plaintext HTTP with no trusted proxy: " +
	"remote-node tokens and cross-node session payloads may be sniffed by any " +
	"passive listener on the wire. A leaked token lets an attacker impersonate " +
	"the remote node and stream arbitrary session data into the primary. " +
	"Terminate TLS upstream and set server.trusted_proxy=true, or bind to " +
	"127.0.0.1 for local-only access."

// trustedProxyXFFReminder is the startup info-level note emitted whenever
// trusted_proxy=true. Pulled out so unit tests can pin the exact text and
// future ops doc references can grep one source of truth. R238-SEC-15 (#848).
const trustedProxyXFFReminder = "trusted_proxy=true: per-IP rate limiters, audit-log " +
	"client_ip fields, and same-origin gates trust the last X-Forwarded-For hop. " +
	"Ensure the upstream proxy (ALB/CloudFront/nginx) strips client-supplied XFF " +
	"headers before appending its own, or applies a hop-count limit — otherwise " +
	"a spoofed XFF can bypass per-IP rate limiting by attributing requests to a " +
	"victim's bucket. naozhi cannot verify the upstream contract from inside the " +
	"process; this reminder is one-shot at startup so the requirement is visible " +
	"in the boot journal."

// shouldWarnReverseNodePlaintext reports whether the /ws-node plaintext warning
// should fire at Server.Start.
//
// Decision matrix:
//
//	no reverse server, any addr, any proxy             → no warn (feature inactive)
//	reverse server,    loopback, any proxy             → no warn (traffic stays on host)
//	reverse server,    public,   trustedProxy=true     → no warn (TLS terminated upstream)
//	reverse server,    public,   trustedProxy=false    → WARN (R176-SEC-MED)
//
// Extracted from Server.Start so a unit test can exercise the matrix without
// binding ports or wiring the full reverse-node subsystem.
func shouldWarnReverseNodePlaintext(reverseServerEnabled bool, trustedProxy bool, addr string) bool {
	if !reverseServerEnabled {
		return false
	}
	if trustedProxy {
		return false
	}
	return isPlaintextPublicAddr(addr)
}

// shouldWarnNoTokenOpen reports whether the "no-auth API open to all callers"
// warning should fire at Server.Start.
//
// Decision matrix (dashboardToken == "" means no auth):
//
//	token set,  any addr, any proxy          → no warn (operator configured auth)
//	token "",   loopback, any proxy          → no warn (only accessible on host)
//	token "",   public,   trustedProxy=true  → no warn (upstream enforces auth)
//	token "",   public,   trustedProxy=false → WARN (R60-SEC-006 + R70-SEC-M1)
//
// Extracted from Server.Start so a unit test can assert the matrix without
// binding ports or mocking slog. R60-SEC-006.
func shouldWarnNoTokenOpen(dashboardToken, addr string, trustedProxy bool) bool {
	if dashboardToken != "" {
		return false
	}
	if !isPlaintextPublicAddr(addr) {
		return false
	}
	if trustedProxy {
		return false
	}
	return true
}

// isPlaintextPublicAddr reports whether addr is a non-loopback TCP listen
// address that would expose Bearer tokens and auth cookies over cleartext
// HTTP. Loopback (127.0.0.1 / ::1 / localhost) is considered safe because
// the traffic never leaves the host. Addresses we cannot parse are treated
// as public so the warning errs on the side of visibility.
func isPlaintextPublicAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// ":8080" form — no host, bound to all interfaces, public by default.
		return true
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// startProjectScanLoop periodically rescans the projects root for added or
// removed subdirectories and cleans up orphaned planner sessions for removed
// projects.
func (s *Server) startProjectScanLoop(ctx context.Context) {
	if s.projectMgr == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(session.ProjectScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				oldNames := s.projectMgr.ProjectNames()
				if err := s.projectMgr.Scan(); err != nil {
					slog.Warn("project rescan", "err", err)
					continue
				}
				newNames := s.projectMgr.ProjectNames()

				// Detect removed projects and clean up orphaned planner sessions
				changed := len(oldNames) != len(newNames)
				for name := range oldNames {
					if _, ok := newNames[name]; !ok {
						changed = true
						plannerKey := project.PlannerKeyFor(name)
						if s.router.Remove(plannerKey) {
							slog.Info("removed orphaned planner", "project", name)
						}
					}
				}
				if changed {
					slog.Info("project list changed", "count", len(newNames))
					if s.hub != nil {
						s.hub.BroadcastSessionsUpdate()
					}
				}
			}
		}
	}()
}
