// connector_rpc.go owns the reverse-RPC method dispatch invoked by handleConn
// for every "request" frame from the primary. Each branch validates inputs,
// calls into router / projectMgr / discovery, and marshals the response via
// marshalResult (connector.go).
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"syscall"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// handleRequest dispatches a reverse-RPC request received from the primary.
//
// Context rule: connCtx is cancelled when handleConn returns (WS drop, ping
// timeout, shutdown) — use it for work that is meaningless once this
// connection ends (send, fetch_events, GetOrCreate, restart_planner) so
// reconnects do not leak goroutines. appCtx is cancelled only when the
// Connector shuts down — takeover and close_discovered use it because the
// CLI child must survive a reconnect while WaitAndCleanup / Takeover run.
// New branches default to connCtx.
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
		if fn := c.loadDiscoverFunc(); fn != nil {
			return fn()
		}
		return marshalResult([]any{})

	case "fetch_discovered_preview":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("fetch_discovered_preview params: %w", err)
		}
		// Defense-in-depth at the RPC boundary (mirrors takeover /
		// close_discovered) so a compromised primary cannot pass ".." /
		// path-traversal session IDs even if the internal check is removed.
		if p.SessionID != "" && !discovery.IsValidSessionID(p.SessionID) {
			return nil, fmt.Errorf("invalid session_id format")
		}
		if fn := c.loadPreviewFunc(); fn != nil {
			return fn(p.SessionID)
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
		sess := c.router.SessionFor(p.Key)
		if sess == nil {
			// %q escapes bidi/C1/newline bytes that would otherwise reach slog
			// on the opposite node via err.Error().
			return nil, fmt.Errorf("session not found: %q", p.Key)
		}
		// #2456: re-admit the watermark ms (same rule as the WS subscribe
		// catch-up) so a same-ms sibling is not lost across the relay.
		return marshalResult(sess.EventEntriesSince(cli.SinceInclusive(p.After)))

	case "fetch_backends":
		// Return THIS node's backend manifest for the primary's node-aware
		// picker. detected is nil: it only feeds a LOCAL dashboard's doctor
		// panel, and probing --version per binary would add a fork-storm to
		// the reverse-RPC hot path. BackendsManifest coerces nil to [].
		return marshalResult(c.router.BackendsManifest(nil))

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
		// Reject oversized text at the trust boundary before it reaches CLI
		// stdin; a compromised primary could otherwise push up to the WS read
		// cap (~16 MB). The cap lives in internal/limits so this package does
		// not import dispatch.
		if n := len(p.Text); n > limits.MaxCoalescedText {
			return nil, fmt.Errorf("send text too long: %d bytes", n)
		}
		opts := session.AgentOpts{}
		if p.Workspace != "" {
			// Syntactic pre-check before Clean/EvalSymlinks: Clean folds
			// `/home/../etc` into `/etc`, defeating a post-Clean prefix check.
			if err := session.ValidateRemoteWorkspacePath(p.Workspace); err != nil {
				return nil, fmt.Errorf("workspace path invalid: %w", err)
			}
			// With no allowed root configured the workspace cannot be bounded;
			// refuse rather than let a compromised primary root a CLI session
			// anywhere (e.g. /etc).
			if c.defaultWorkspace == "" {
				return nil, fmt.Errorf("workspace overrides disabled: no allowed root configured on this node")
			}
			ws, err := c.sanitizeWorkspacePath(p.Workspace, "workspace", false)
			if err != nil {
				return nil, err
			}
			opts.Workspace = ws
		}
		sess, _, err := c.router.GetOrCreate(connCtx, p.Key, opts)
		if err != nil {
			return nil, fmt.Errorf("get session: %w", err)
		}
		// Send is async: the primary subscribed before sending, so events arrive
		// via streamEvents. connCtx lets a relay disconnect cancel in-flight
		// sends; wg makes a dropped connection wait for them before teardown.
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
					// The RPC already returned "accepted", so surface the failure
					// into this session's EventLog for subscribed dashboards. The
					// error text comes from a remote transport stack and is
					// broadcast to WS clients + persisted, so sanitize it (log
					// injection) and cap at 512 bytes; full detail is in slog above.
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
			// Syntactic pre-check always — traversal / control bytes / relative
			// paths must not reach filepath.Clean.
			if err := session.ValidateRemoteWorkspacePath(cwd); err != nil {
				return nil, fmt.Errorf("takeover cwd invalid: %w", err)
			}
			// No allowed root configured: refuse the cwd override (same policy
			// as "send").
			if c.defaultWorkspace == "" {
				return nil, fmt.Errorf("takeover cwd overrides disabled: no allowed root configured on this node")
			}
			// Takeover does not tolerate ENOENT: the shim is still alive in the directory.
			cleanCWD, err := c.sanitizeWorkspacePath(cwd, "takeover cwd", false)
			if err != nil {
				return nil, err
			}
			cwd = cleanCWD
		}
		cwdKey := session.SanitizeCWDKey(cwd)
		key := session.TakeoverKey(cwdKey)
		pid, sessionID, procStartTime, reqCWD, claudeDir := p.PID, p.SessionID, p.ProcStartTime, p.CWD, c.claudeDir
		// wg keeps reconnect waiting for in-flight cleanup; appCtx so a
		// transient connection drop does not abort cleanup already in progress.
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
			// Empty AgentOpts by design: the remote node has no agents registry,
			// so per-agent overrides (model / args / system_prompt, #2493) do
			// not cross the node boundary on takeover.
			if _, err := c.router.Takeover(appCtx, key, sessionID, cwd, session.AgentOpts{}); err != nil {
				slog.Debug("connector takeover failed", "key", key, "err", err)
			}
		}()
		return marshalResult(map[string]string{"status": "accepted", "key": key})

	case "close_discovered":
		// Proxied from the primary's handleClose: the primary already verified
		// PID ∈ discovered and the caller is an authenticated node.
		// ProcStartTime still guards against PID reuse.
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
		// CWD feeds discovery.WaitAndCleanup, which derives a lockDir and
		// os.RemoveAll's it. Reject syntactic traversal / control bytes /
		// relative paths up front; when defaultWorkspace is configured also
		// enforce the EvalSymlinks + allowed-root check takeover performs. Empty
		// defaultWorkspace falls back to syntactic-only validation (single-node).
		if p.CWD != "" {
			if err := session.ValidateRemoteWorkspacePath(p.CWD); err != nil {
				return nil, fmt.Errorf("close_discovered cwd invalid: %w", err)
			}
			if c.defaultWorkspace != "" {
				// close_discovered often runs after the CLI exited and its CWD is
				// gone: tolerateMissing falls back to the cleaned path while
				// still enforcing IsAbs + allowed-root.
				cleanCWD, err := c.sanitizeWorkspacePath(p.CWD, "close_discovered cwd", true)
				if err != nil {
					return nil, err
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
		// Validate project_name at the trust boundary so bidi/C1/newline bytes
		// never reach ErrNotFound / slog attrs on the miss path.
		if err := project.ValidateProjectName(p.ProjectName); err != nil {
			return nil, fmt.Errorf("restart_planner: %w", err)
		}
		// The resolver derives planner-view opts without inheriting defaults
		// (docs/rfc/key-resolver.md §2.2, #7); the inline path serves
		// headless/test callers without a resolver.
		var plannerKey string
		var opts session.AgentOpts
		if c.resolver != nil {
			key, plannerOpts, ok := c.resolver.ResolveForPlannerKey(p.ProjectName)
			if !ok {
				// %q so bytes in the primary-supplied name cannot forge
				// structured-log fields when the remote side logs this error.
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
				opts.SystemPrompt = prompt // #2493: dedicated field, not ExtraArgs
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
		// Validate project_name up front so the ErrNotFound / ValidateConfig
		// error paths never log attacker-controlled bidi / newline bytes.
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
		// primary must not push unbounded prompts, NUL-truncated argv, or
		// flag-injected model names through the reverse-RPC trust boundary.
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
		// Prefer the non-destructive control_request path: raw SIGINT kills
		// Claude `-p` outright, forcing a fresh spawn (losing resume context and
		// leaking socket files). Matches the dashboard HTTP / WS handlers.
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
		// Full validation (length + UTF-8 + C0/C1 control gate) again on the
		// server-role node: defends against a compromised control node shipping
		// log-injection / terminal-corruption bytes.
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
		// Validate project_name at the trust boundary (mirrors restart_planner
		// / update_config).
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
		// %q so bidi/C1/newline bytes in a primary-injected method name are
		// escaped before the remote logs the error string.
		return nil, fmt.Errorf("unknown method: %q", req.Method)
	}
}
