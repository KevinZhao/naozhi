// connector_rpc.go owns the reverse-RPC method dispatch — the 18-case
// switch invoked by handleConn for every "request" frame received from
// the primary. Each branch validates inputs, calls into router /
// projectMgr / discovery / dispatch, and marshals the response via the
// marshalResult helper that lives in connector.go. Connection lifecycle
// (write loop, ping, drain budget, subscribe state) lives in
// connector_conn.go; live event streaming lives in connector_subscribe.go.
// Split is purely organisational — all three files are package upstream.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// handleRequest dispatches a reverse-RPC request received from the primary.
//
// Context selection matrix (RNEW-008):
//
//   - connCtx ("connection-scoped"): cancelled when handleConn returns
//     (WebSocket drop, ping timeout, graceful shutdown). Use for any work
//     whose result is meaningless after this connection ends, so
//     reconnects do not leak goroutines. Examples: `send` stream waits,
//     synchronous `fetch_events`, `router.GetOrCreate` called on the
//     RPC's behalf.
//
//   - appCtx ("app-scoped"): cancelled only when the Connector shuts
//     down entirely. Use when the work MUST outlive the current WS
//     connection — typically takeover / discovery waits where the
//     CLI child process is expected to survive reconnects.
//
// New RPC branches: default to connCtx. Only switch to appCtx when you
// can justify in a comment why cross-reconnect persistence is required.
func (c *Connector) handleRequest(appCtx, connCtx context.Context, req node.ReverseMsg, wg *sync.WaitGroup) (json.RawMessage, error) {
	switch req.Method {
	case "fetch_sessions":
		return marshalResult(c.router.ListSessions())

	case "fetch_projects":
		if c.projMgr == nil {
			return marshalResult([]any{})
		}
		return marshalResult(c.projMgr.All())

	case "fetch_discovered":
		if c.discoverFunc != nil {
			return c.discoverFunc()
		}
		return marshalResult([]any{})

	case "fetch_discovered_preview":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("fetch_discovered_preview params: %w", err)
		}
		// Defense-in-depth: the HTTP dashboard path validates on the
		// control-node side and `discovery.LoadHistoryChainTailCtx` also
		// validates internally, but validating here at the RPC boundary
		// mirrors the `takeover` / `close_discovered` handlers and prevents
		// a future refactor from removing the internal check and exposing
		// `{".."}` / path-traversal inputs from a compromised primary.
		// R65-SEC-M-1.
		if p.SessionID != "" && !discovery.IsValidSessionID(p.SessionID) {
			return nil, fmt.Errorf("invalid session_id format")
		}
		if c.previewFunc != nil {
			return c.previewFunc(p.SessionID)
		}
		return marshalResult([]any{})

	case "fetch_events":
		var p struct {
			Key   string `json:"key"`
			After int64  `json:"after"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("fetch_events params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("fetch_events key: %w", err)
		}
		sess := c.router.GetSession(p.Key)
		if sess == nil {
			// R180-SEC-M1 / R180-GO-P1: %q escapes any bidi/C1/newline bytes
			// that ValidateSessionKey would accept but would break log
			// parsing / bidi-flip the terminal if they reach slog via the
			// returned err.Error() on the opposite node.
			return nil, fmt.Errorf("session not found: %q", p.Key)
		}
		return marshalResult(sess.EventEntriesSince(p.After))

	case "send":
		var p struct {
			Key       string `json:"key"`
			Text      string `json:"text"`
			Workspace string `json:"workspace"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("send params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("send key: %w", err)
		}
		// Reject oversized text at the reverse-RPC trust boundary before it
		// reaches sess.Send → CoalesceMessages. Without this a compromised or
		// misconfigured primary could push up to ~16 MB (the WS read cap)
		// straight into CLI stdin, relying only on the shim's 12 MB line
		// ceiling to reject it. Matches the primary-side dashboard cap
		// chain (maxWSSendTextBytes=1 MB → coalesce soft cap 4 MB → shim
		// 12 MB). R68-SEC-H1.
		//
		// Source of truth lives in internal/limits so this reverse-RPC
		// trust boundary doesn't have to import dispatch (R228-ARCH-9).
		if n := len(p.Text); n > limits.MaxCoalescedText {
			return nil, fmt.Errorf("send text too long: %d bytes", n)
		}
		opts := session.AgentOpts{}
		if p.Workspace != "" {
			// Syntactic pre-check before filepath.Clean/EvalSymlinks. Clean
			// silently folds `/home/../etc` into `/etc`, so a post-Clean
			// prefix check under an empty defaultWorkspace would let any
			// absolute path through on single-user deployments. R68-SEC-M2.
			if err := session.ValidateRemoteWorkspacePath(p.Workspace); err != nil {
				return nil, fmt.Errorf("workspace path invalid: %w", err)
			}
			// When no allowed root is configured (defaultWorkspace=="") on this
			// reverse node, we cannot bound the workspace to any prefix. A
			// compromised/misconfigured primary could otherwise push any
			// absolute path (e.g. `/etc`) and spawn a CLI session rooted
			// there. Refuse rather than trust. R68-SEC-M2.
			if c.defaultWorkspace == "" {
				return nil, fmt.Errorf("workspace overrides disabled: no allowed root configured on this node")
			}
			// Sanitize workspace path to prevent directory traversal via symlinks.
			ws, err := filepath.EvalSymlinks(filepath.Clean(p.Workspace))
			if err != nil {
				return nil, fmt.Errorf("workspace path invalid: %w", err)
			}
			if !filepath.IsAbs(ws) {
				return nil, fmt.Errorf("workspace must be absolute path")
			}
			if ws != c.defaultWorkspace &&
				!strings.HasPrefix(ws, c.defaultWorkspace+string(filepath.Separator)) {
				return nil, fmt.Errorf("workspace %q outside allowed root %q", ws, c.defaultWorkspace)
			}
			opts.Workspace = ws
		}
		sess, _, err := c.router.GetOrCreate(connCtx, p.Key, opts)
		if err != nil {
			return nil, fmt.Errorf("get session: %w", err)
		}
		// Send is async: primary subscribed before sending, events arrive via streamEvents.
		// Use connCtx so a relay disconnect cancels in-flight sends, preventing
		// goroutine accumulation across reconnect cycles. Register with the
		// handleConn waitgroup so a dropped connection waits for in-flight
		// sends to return before tearing down subscriptions.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("connector send panic", "key", p.Key, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			if _, err := sess.Send(connCtx, p.Text, nil, nil); err != nil {
				if connCtx.Err() == nil {
					slog.Warn("connector send failed", "key", p.Key, "err", err)
					// R49-REL-CONNECTOR-SEND-RESULT-LOSS: the RPC already
					// returned {"status":"accepted"} to primary, so a plain
					// log.Warn leaves the UI showing "sent" while the message
					// actually failed. Inject a system event into this
					// session's EventLog so subscribed dashboards surface
					// the failure on the next event push. Keep the message
					// compact and classifier-friendly; full detail stays
					// in the slog line above.
					//
					// R172-SEC-M4: err.Error() originates from a remote /
					// transport stack — it may contain C1 controls, bidi
					// overrides, or LS/PS characters that byte-level
					// `< 0x20` gates miss. The summary is broadcast to every
					// dashboard WS client subscribed to this session AND
					// appended to persistedHistory (journalctl / sessions
					// persistence), so an unsanitized error string here is a
					// log-injection primitive that can flip terminal output
					// under `tail -f` for operators and pollute dashboard
					// rendering. Sanitize through osutil.SanitizeForLog and
					// cap at 512 bytes — long remote stack traces beyond
					// that point add noise without diagnostic value (full
					// detail is already in the slog.Warn above).
					sess.LogSystemEvent("发送失败：" + osutil.SanitizeForLog(err.Error(), 512))
				}
			}
		}()
		return marshalResult(map[string]string{"status": "accepted"})

	case "takeover":
		var p struct {
			PID           int    `json:"pid"`
			SessionID     string `json:"session_id"`
			CWD           string `json:"cwd"`
			ProcStartTime uint64 `json:"proc_start_time"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("takeover params: %w", err)
		}
		if p.PID <= 0 || p.SessionID == "" {
			return nil, fmt.Errorf("pid and session_id are required")
		}
		if !discovery.IsValidSessionID(p.SessionID) {
			return nil, fmt.Errorf("invalid session_id format")
		}
		if p.ProcStartTime == 0 {
			return nil, fmt.Errorf("proc_start_time is required")
		}
		actual, err := discovery.ProcStartTime(p.PID)
		if err != nil {
			return nil, fmt.Errorf("cannot verify process identity for pid %d: %w", p.PID, err)
		}
		if actual != p.ProcStartTime {
			return nil, fmt.Errorf("process identity mismatch (pid %d may have been reused)", p.PID)
		}
		if err := osutil.SendTerm(p.PID); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				return nil, fmt.Errorf("kill process %d: %w", p.PID, err)
			}
		}
		cwd := p.CWD
		if cwd == "" {
			cwd = "unknown"
		}
		// Validate CWD against workspace root (same check as "send" RPC).
		if cwd != "unknown" {
			// Syntactic pre-check always — even with empty defaultWorkspace,
			// `..` traversal / control bytes / non-absolute paths have no
			// business reaching filepath.Clean. R68-SEC-M2.
			if err := session.ValidateRemoteWorkspacePath(cwd); err != nil {
				return nil, fmt.Errorf("takeover cwd invalid: %w", err)
			}
			// When no allowed root is configured on this reverse node, refuse
			// the cwd override — the takeover would otherwise spawn a CLI
			// session rooted wherever the primary pointed. Aligns with the
			// "send" RPC above so both call sites have the same policy.
			// R68-SEC-M2.
			if c.defaultWorkspace == "" {
				return nil, fmt.Errorf("takeover cwd overrides disabled: no allowed root configured on this node")
			}
			cleanCWD, err := filepath.EvalSymlinks(filepath.Clean(cwd))
			if err != nil {
				return nil, fmt.Errorf("takeover cwd path invalid: %w", err)
			}
			if !filepath.IsAbs(cleanCWD) {
				return nil, fmt.Errorf("takeover cwd must be absolute path")
			}
			if cleanCWD != c.defaultWorkspace &&
				!strings.HasPrefix(cleanCWD, c.defaultWorkspace+string(filepath.Separator)) {
				return nil, fmt.Errorf("takeover cwd %q outside allowed root %q", cleanCWD, c.defaultWorkspace)
			}
			cwd = cleanCWD
		}
		cwdKey := session.SanitizeCWDKey(cwd)
		key := session.TakeoverKey(cwdKey)
		pid, sessionID, procStartTime, reqCWD, claudeDir := p.PID, p.SessionID, p.ProcStartTime, p.CWD, c.claudeDir
		// Track with connection wg so reconnect waits for in-flight cleanup rather
		// than letting goroutines pile up across reconnect cycles. Use appCtx so a
		// transient connection drop does not abort cleanup already in progress;
		// appCtx outlives connCtx, but wg keeps accounting honest.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("connector takeover panic", "key", key, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			discovery.WaitAndCleanup(appCtx, pid, procStartTime, claudeDir, reqCWD, sessionID)
			if appCtx.Err() != nil {
				return // connector shutting down
			}
			if _, err := c.router.Takeover(appCtx, key, sessionID, cwd, session.AgentOpts{}); err != nil {
				slog.Debug("connector takeover failed", "key", key, "err", err)
			}
		}()
		return marshalResult(map[string]string{"status": "accepted", "key": key})

	case "close_discovered":
		// Proxied from primary's handleClose — no discovered-cache check here:
		// the primary already verified PID ∈ discovered before forwarding, and
		// the RPC caller is an authenticated node. ProcStartTime still guards
		// against PID reuse between primary's check and this kill.
		var p struct {
			PID           int    `json:"pid"`
			SessionID     string `json:"session_id"`
			CWD           string `json:"cwd"`
			ProcStartTime uint64 `json:"proc_start_time"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("close_discovered params: %w", err)
		}
		if p.PID <= 0 {
			return nil, fmt.Errorf("pid is required")
		}
		if p.ProcStartTime == 0 {
			return nil, fmt.Errorf("proc_start_time is required")
		}
		if p.SessionID != "" && !discovery.IsValidSessionID(p.SessionID) {
			return nil, fmt.Errorf("invalid session_id format")
		}
		// CWD flows into discovery.WaitAndCleanup which builds a lockDir path
		// and os.RemoveAll it (protected by filepath.Rel sandbox, but we still
		// reject syntactic `..` traversal / control bytes / non-absolute paths
		// up front to avoid depending on a single defense layer). Parallels the
		// takeover-side check at line 520. R-close-discovered-cwd-validate.
		//
		// When defaultWorkspace is configured, additionally enforce the same
		// EvalSymlinks + allowedRoot prefix check that takeover performs. This
		// closes the gap called out in the 2026-05-08 security review: without
		// it a primary could point close_discovered at a CWD outside the
		// reverse node's configured root, and WaitAndCleanup would derive the
		// lockDir from that path. When defaultWorkspace is empty we fall back
		// to syntactic-only validation to preserve compatibility with single-
		// node deployments that never configure an allowed root.
		if p.CWD != "" {
			if err := session.ValidateRemoteWorkspacePath(p.CWD); err != nil {
				return nil, fmt.Errorf("close_discovered cwd invalid: %w", err)
			}
			if c.defaultWorkspace != "" {
				// Unlike takeover (which expects the CWD to exist because
				// the shim is still running inside it), close_discovered
				// frequently runs AFTER the Claude CLI has exited and the
				// working directory may already be gone. Treat ENOENT as
				// "not a symlink attack, path just vanished" — fall back to
				// the cleaned syntactic path and still enforce the allowed-
				// root prefix check so a relocated-but-existed attacker
				// payload like "/etc/passwd" cannot slip through.
				cleaned := filepath.Clean(p.CWD)
				cleanCWD, err := filepath.EvalSymlinks(cleaned)
				if err != nil {
					if !errors.Is(err, fs.ErrNotExist) {
						return nil, fmt.Errorf("close_discovered cwd path invalid: %w", err)
					}
					cleanCWD = cleaned
				}
				if !filepath.IsAbs(cleanCWD) {
					return nil, fmt.Errorf("close_discovered cwd must be absolute path")
				}
				if cleanCWD != c.defaultWorkspace &&
					!strings.HasPrefix(cleanCWD, c.defaultWorkspace+string(filepath.Separator)) {
					return nil, fmt.Errorf("close_discovered cwd %q outside allowed root %q", cleanCWD, c.defaultWorkspace)
				}
				p.CWD = cleanCWD
			}
		}
		actual, err := discovery.ProcStartTime(p.PID)
		if err != nil {
			return nil, fmt.Errorf("cannot verify process identity for pid %d: %w", p.PID, err)
		}
		if actual != p.ProcStartTime {
			return nil, fmt.Errorf("process identity mismatch (pid %d may have been reused)", p.PID)
		}
		if err := osutil.SendTerm(p.PID); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				return nil, fmt.Errorf("kill process %d: %w", p.PID, err)
			}
		}
		pid, sessionID, procStartTime, cwd, claudeDir := p.PID, p.SessionID, p.ProcStartTime, p.CWD, c.claudeDir
		// Track with connection wg so reconnect waits for this cleanup to finish.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("connector close_discovered panic", "pid", pid, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			if appCtx.Err() != nil {
				return
			}
			discovery.WaitAndCleanup(appCtx, pid, procStartTime, claudeDir, cwd, sessionID)
		}()
		return marshalResult(map[string]string{"status": "ok"})

	case "restart_planner":
		var p struct {
			ProjectName string `json:"project_name"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("restart_planner params: %w", err)
		}
		// R181-SEC-P2-2: validate project_name at the trust boundary so
		// bidi/C1/newline bytes never reach ErrNotFound / slog attrs on
		// the miss path. Consistent with update_config below.
		if err := project.ValidateProjectName(p.ProjectName); err != nil {
			return nil, fmt.Errorf("restart_planner: %w", err)
		}
		// Delegate planner-view opts derivation to Resolver when wired;
		// preserves the "do not inherit defaults" contract of
		// administrative planner restarts (docs/rfc/key-resolver.md §2.2
		// #7). Legacy inlined path retained for headless/test callers.
		var plannerKey string
		var opts session.AgentOpts
		if c.resolver != nil {
			key, plannerOpts, ok := c.resolver.ResolveForPlannerKey(p.ProjectName)
			if !ok {
				// Use %q so bidi/C1/newline bytes in the primary-supplied
				// name cannot forge structured-log fields when the remote
				// side logs this error. R177-SEC-2.
				return nil, fmt.Errorf("project not found: %q", p.ProjectName)
			}
			plannerKey = key
			opts = plannerOpts
		} else {
			if c.projMgr == nil {
				return nil, fmt.Errorf("projects not configured")
			}
			proj := c.projMgr.Get(p.ProjectName)
			if proj == nil {
				return nil, fmt.Errorf("project not found: %q", p.ProjectName)
			}
			plannerKey = proj.PlannerSessionKey()
			opts = session.AgentOpts{
				Model:     c.projMgr.EffectivePlannerModel(proj),
				Workspace: proj.Path,
				Exempt:    true,
			}
			if prompt := c.projMgr.EffectivePlannerPrompt(proj); prompt != "" {
				opts.ExtraArgs = []string{"--append-system-prompt", prompt}
			}
		}
		if _, err := c.router.ResetAndRecreate(connCtx, plannerKey, opts); err != nil {
			return nil, fmt.Errorf("restart planner: %w", err)
		}
		return marshalResult(map[string]string{"status": "restarted"})

	case "update_config":
		var p struct {
			ProjectName string          `json:"project_name"`
			Config      json.RawMessage `json:"config"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("update_config params: %w", err)
		}
		// R181-SEC-P2-2: validate project_name up front so the surrounding
		// ErrNotFound (now %q-escaped per project/manager.go:228) and
		// ValidateConfig error paths never log attacker-controlled bidi /
		// newline bytes.
		if err := project.ValidateProjectName(p.ProjectName); err != nil {
			return nil, fmt.Errorf("update_config: %w", err)
		}
		if c.projMgr == nil {
			return nil, fmt.Errorf("projects not configured")
		}
		var cfg project.ProjectConfig
		if err := json.Unmarshal(p.Config, &cfg); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
		// Same validation the dashboard HTTP handler enforces: a compromised
		// or misconfigured primary must not be able to push unbounded prompts,
		// NUL-truncated argv, or flag-injected model names through the
		// reverse-RPC trust boundary. R68-SEC-H2.
		// R180-GO-P1: wrap to match the surrounding handleRequest style
		// (every other error return uses "<method>: %w") so caller slog
		// attrs identify which RPC method triggered the validation failure.
		if err := project.ValidateConfig(cfg); err != nil {
			return nil, fmt.Errorf("update_config validate: %w", err)
		}
		if err := c.projMgr.UpdateConfig(p.ProjectName, cfg); err != nil {
			return nil, fmt.Errorf("update config: %w", err)
		}
		return marshalResult(map[string]string{"status": "ok"})

	case "remove_session":
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("remove_session params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("remove_session key: %w", err)
		}
		removed := c.router.Remove(p.Key)
		return marshalResult(map[string]bool{"removed": removed})

	case "interrupt_session":
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("interrupt_session params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("interrupt_session key: %w", err)
		}
		// Prefer the non-destructive control_request path so the CLI
		// subprocess survives. Raw SIGINT via InterruptSession kills Claude
		// `-p` outright, which tears down the shim and forces a brand-new
		// spawn on the next message (losing resume context and leaking
		// socket files). Matches the dashboard HTTP / WS handlers. R67-GO-2.
		outcome := c.router.InterruptSessionSafe(p.Key)
		interrupted := outcome == session.InterruptSent
		return marshalResult(map[string]bool{"interrupted": interrupted})

	case "set_session_label":
		var p struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("set_session_label params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("set_session_label key: %w", err)
		}
		// Full validation (length + UTF-8 + C0/C1 control gate) via the
		// shared validator. The dashboard-facing HTTP path already validates
		// on the control-node side; this second check defends the
		// server-role node against a compromised control-node shipping
		// labels with log-injection or terminal-corruption bytes directly
		// to the reverse-RPC worker. R64-GO-H3 / L1.
		label, err := session.ValidateUserLabel(p.Label)
		if err != nil {
			return nil, fmt.Errorf("set_session_label label: %w", err)
		}
		updated := c.router.SetUserLabel(p.Key, label)
		return marshalResult(map[string]bool{"updated": updated})

	case "set_favorite":
		var p struct {
			ProjectName string `json:"project_name"`
			Favorite    bool   `json:"favorite"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("set_favorite params: %w", err)
		}
		// R182-SEC-M1: mirror restart_planner / update_config — validate
		// project_name at the trust boundary so bidi/C1/newline bytes
		// cannot reach ErrNotFound wrap (see manager.go:208 also upgraded
		// to %q for defense-in-depth) or subsequent slog attrs.
		if err := project.ValidateProjectName(p.ProjectName); err != nil {
			return nil, fmt.Errorf("set_favorite: %w", err)
		}
		if c.projMgr == nil {
			return nil, fmt.Errorf("projects not configured")
		}
		if err := c.projMgr.SetFavorite(p.ProjectName, p.Favorite); err != nil {
			return nil, fmt.Errorf("set favorite: %w", err)
		}
		return marshalResult(map[string]any{"status": "ok", "favorite": p.Favorite})

	default:
		// %q so any bidi/C1/newline bytes in a primary-injected method name
		// are escaped rather than propagating verbatim into the error
		// string that the remote logs. R177-SEC-2.
		return nil, fmt.Errorf("unknown method: %q", req.Method)
	}
}
