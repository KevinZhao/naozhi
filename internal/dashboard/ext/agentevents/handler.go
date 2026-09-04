package agentevents

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dashboard/contracts"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// Agent-team dashboard endpoints (RFC v4 agent-team-ui §3.5), behind the same
// auth middleware and remote-node proxy fallback as /api/sessions/events.
//
//   GET /api/sessions/agent_events?key=&node=&task_id=&after=<ms>&limit=
//     → 200 [EventEntry...] (Time >= after) | 202 pending (linker not yet
//       resolved) | 404 unknown task | 400 bad param
//   GET /api/sessions/tool_result?key=&node=&path=tool-results/<id>.ext
//     → 200 text/plain | 404 no linker/file/traversal | 400 | 413 > cap

// taskIDRe bounds task_id to CLI's observed shapes (prefix + base36); the
// prefix is deliberately not enforced, length + alphabet suffice.
var taskIDRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// toolResultPathRe mirrors TranscriptReader.extractPersistedPath's
// "tool-results/<basename>" shape; the cli side re-enforces the alphabet.
var toolResultPathRe = regexp.MustCompile(`^tool-results/[A-Za-z0-9]{1,32}\.(txt|json|log)$`)

const (
	maxAgentEventsLimit = 500
	// toolResultMaxBytes caps a served tool-result file; it is one stream-json
	// line, so it shares the upstream line cap (#2084).
	toolResultMaxBytes = limits.MaxStreamJSONLine
)

// Handler hosts the agent-team endpoints, kept separate from SessionHandlers
// so the auth middleware wiring in dashboard.go stays grep-able. linkerFor
// (agentlink.AgentLinker) is injectable so tests stub the lookup without a
// live *cli.Process; production uses linkerForSession.
type Handler struct {
	router      *session.Router
	nodeAccess  NodeAccessor
	linkerFor   func(key string) agentlink.AgentLinker
	allowedRoot string // EvalSymlinks-resolved ~/.claude/projects; set by New
}

// linkerForSession is the default lookup (ManagedSession → *cli.Process →
// SubagentLinker); nil when the session is missing, dead, or non-claude.
func (h *Handler) linkerForSession(key string) agentlink.AgentLinker {
	if h.linkerFor != nil {
		return h.linkerFor(key)
	}
	sess := h.router.SessionFor(key)
	if sess == nil {
		return nil
	}
	// A typed-nil concrete pointer would become a non-nil interface; guard it
	// first to keep the "no live linker → 404" contract.
	concrete := sess.SubagentLinker()
	if concrete == nil {
		return nil
	}
	return concrete
}

func (h *Handler) HandleAgentEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("key")
	if err := session.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}
	taskID := q.Get("task_id")
	if !taskIDRe.MatchString(taskID) {
		http.Error(w, "invalid task_id parameter", http.StatusBadRequest)
		return
	}

	afterStr := q.Get("after")
	limitStr := q.Get("limit")

	var (
		after int64
		limit int = 200
	)
	if afterStr != "" {
		v, err := strconv.ParseInt(afterStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid after parameter", http.StatusBadRequest)
			return
		}
		after = v
	}
	if limitStr != "" {
		v, err := strconv.Atoi(limitStr)
		if err != nil || v < 0 {
			http.Error(w, "invalid limit parameter", http.StatusBadRequest)
			return
		}
		if v == 0 || v > maxAgentEventsLimit {
			v = maxAgentEventsLimit
		}
		limit = v
	}

	// Remote node proxy parity with /api/sessions/events. A peer binary that
	// predates the feature returns "unknown method"; degrade to 404 so the UI
	// shows "no recorded internals" instead of a 502.
	if nodeID := q.Get("node"); nodeID != "" && nodeID != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return
		}
		entries, err := nc.FetchEvents(r.Context(), key, after)
		_ = entries
		if err != nil {
			if contracts.IsUnknownRPCMethodErr(err) {
				http.Error(w, "unknown task", http.StatusNotFound)
				return
			}
			slog.Warn("remote fetch agent_events failed", "node", nodeID, "key", key, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		// Remote peers do not yet expose agent_events fan-out: graceful 404.
		http.Error(w, "unknown task", http.StatusNotFound)
		return
	}

	linker := h.linkerForSession(key)
	if linker == nil {
		http.Error(w, "unknown task", http.StatusNotFound)
		return
	}

	info, ok := linker.QueryOrResolveFast(taskID)
	if !ok {
		// Linker context not yet installed (awaiting first live init event):
		// tell the client to retry; switchAgentView bounds the retry loop.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"pending"}`))
		return
	}
	if info.InternalAgentID == "" || info.JSONLPath == "" {
		http.Error(w, "unknown task", http.StatusNotFound)
		return
	}

	// Defence in depth at the HTTP boundary: JSONLPath must be under the
	// allowed root (SeedFromHistory already validates cli-side). Empty
	// allowedRoot (first run) fails closed to 404.
	if h.allowedRoot != "" && !jsonlPathUnderAllowedRoot(info.JSONLPath, h.allowedRoot) {
		slog.Warn("agent_events: JSONLPath outside allowed root, rejecting",
			"path", info.JSONLPath, "allowed_root", h.allowedRoot)
		http.Error(w, "unknown task", http.StatusNotFound)
		return
	}

	reader := cli.NewTranscriptReader(info.JSONLPath)
	defer reader.Close()
	// Re-admit the `after` millisecond (as /api/sessions/events): consecutive
	// transcript lines can share a timestamp, so a strict `>` cursor lost
	// siblings cut off by the previous limit; agent_view.js dedups (#2432).
	entries, err := reader.Read(cli.SinceInclusive(after), limit)
	if err != nil {
		if os.IsNotExist(err) {
			// CLI may have pruned the jsonl (e.g. /new on the parent): 404 → "no record" toast.
			http.Error(w, "unknown task", http.StatusNotFound)
			return
		}
		slog.Warn("agent_events: transcript read failed", "path", info.JSONLPath, "err", err)
		http.Error(w, "transcript read error", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		// Must serialise as `[]`, not `null`: agent_view.js treats a falsy body
		// as "no data" and never subscribes to the live feed.
		entries = []clievent.EventEntry{}
	}
	httputil.WriteJSON(w, entries)
}

func (h *Handler) HandleToolResult(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("key")
	if err := session.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}
	rel := q.Get("path")
	if !toolResultPathRe.MatchString(rel) {
		http.Error(w, "invalid path parameter", http.StatusBadRequest)
		return
	}

	if nodeID := q.Get("node"); nodeID != "" && nodeID != "local" {
		// Tool-result fetches do not cross nodes (file lives on the node that
		// ran the CLI); 404 is the contract.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	linker := h.linkerForSession(key)
	if linker == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	root := linker.ProjectSessionDir()
	if root == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	cleaned := filepath.Clean(rel)
	if !strings.HasPrefix(cleaned, "tool-results/") {
		http.Error(w, "invalid path parameter", http.StatusBadRequest)
		return
	}
	abs := filepath.Join(root, filepath.FromSlash(cleaned))
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Resolve root like the leaf so the prefix check is consistent (macOS /private).
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = resolvedRoot
	}
	toolResultsRoot := filepath.Join(root, "tool-results")
	if !strings.HasPrefix(resolved, toolResultsRoot+string(filepath.Separator)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// O_NOFOLLOW open so the streamed bytes come from the inode validated via
	// Fstat below (closes the symlink-swap TOCTOU after EvalSymlinks). Every
	// open error folds to 404 so "missing" and "escape attempt" look identical.
	f, err := dashproject.OpenWorkspaceFile(resolved)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	// Fstat the fd so size/IsDir reflect the inode we read, not a swapped name.
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if info.Size() > toolResultMaxBytes {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, f); err != nil {
		slog.Debug("tool_result: copy truncated", "err", err)
	}
}

// Deps bundles all wiring for New.
type Deps struct {
	Router     *session.Router
	NodeAccess NodeAccessor
}

// New constructs a Handler, resolving ~/.claude/projects once as the canonical root.
func New(d Deps) *Handler {
	root := claudeProjectsAllowedRoot()
	return &Handler{
		router:      d.Router,
		nodeAccess:  d.NodeAccess,
		allowedRoot: root,
	}
}

// claudeProjectsAllowedRoot returns the EvalSymlinks-resolved ~/.claude/projects,
// or the lexical path when resolution fails (first run) so checks degrade, not reject.
func claudeProjectsAllowedRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	raw := filepath.Join(home, ".claude", "projects")
	if resolved, err := filepath.EvalSymlinks(raw); err == nil {
		return resolved
	}
	return raw
}

// jsonlPathUnderAllowedRoot checks that p is anchored under root after
// resolving p's nearest existing ancestor. Anchors on root + separator
// (plain HasPrefix would match "/var/fooBar" for "/var/foo") and returns
// false for an empty root to fail safe.
func jsonlPathUnderAllowedRoot(p, root string) bool {
	if root == "" {
		return false
	}
	abs := filepath.Clean(p)
	if !filepath.IsAbs(abs) {
		return false
	}
	// Resolve the nearest existing ancestor so a not-yet-written jsonl (CLI
	// emits the path before the first write) still resolves through symlinks.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	} else {
		cur := abs
		tail := ""
		for {
			parent := filepath.Dir(cur)
			if parent == cur {
				// Filesystem root reached: keep the unresolved lexical path.
				break
			}
			base := filepath.Base(cur)
			if tail == "" {
				tail = base
			} else {
				tail = filepath.Join(base, tail)
			}
			cur = parent
			if resolved, err2 := filepath.EvalSymlinks(cur); err2 == nil {
				abs = filepath.Join(resolved, tail)
				break
			}
		}
	}
	if abs == root {
		return false // exact root match is not under root
	}
	return strings.HasPrefix(abs, root+string(filepath.Separator))
}
