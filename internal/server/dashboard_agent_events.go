package server

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
	"github.com/naozhi/naozhi/internal/session"
)

// Agent-team dashboard endpoints (RFC v4 agent-team-ui §3.5).
//
//   GET /api/sessions/agent_events
//     ?key=<session_key>&node=<node>&task_id=<t...>&after=<ms>&limit=<n>
//   → 200 [EventEntry...]            chronological transcript slice
//   → 202 {"status":"pending"}       SubagentLinker has not resolved yet
//   → 404 "unknown task"             tombstone / no live linker
//   → 400                            query param validation failure
//
//   GET /api/sessions/tool_result
//     ?key=<session_key>&node=<node>&path=tool-results/<id>.ext
//   → 200 text/plain
//   → 404                            no linker / no file / traversal
//   → 400                            path whitelist violation
//   → 413                            > toolResultMaxBytes
//
// Both endpoints run under the same auth middleware and the same remote-node
// proxy fallback as /api/sessions/events.

// taskIDRe bounds the task_id query parameter to CLI's observed shapes:
// "t" or "b" prefix + 7-12 base36 chars. The prefix is not enforced so
// future CLI changes don't require a parser update; length + alphabet
// whitelist suffices (RFC §4).
var taskIDRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// toolResultPathRe mirrors the persisted_path shape emitted by
// TranscriptReader.extractPersistedPath: "tool-results/<basename>". The
// basename alphabet + extension list are enforced again in
// toolResultBasenameRe (internal/cli/subagent_transcript.go) — belt-and-
// suspenders against filesystem traversal.
var toolResultPathRe = regexp.MustCompile(`^tool-results/[A-Za-z0-9]{1,32}\.(txt|json|log)$`)

const (
	maxAgentEventsLimit = 500
	toolResultMaxBytes  = 16 << 20 // 16 MB
)

// AgentEventsHandlers hosts the agent-team endpoints. Kept separate from
// SessionHandlers so the auth middleware wiring in dashboard.go stays grep-able.
//
// linkerFor is injected so tests can stub the router/ManagedSession lookup
// chain without a live *cli.Process. Production code uses linkerForSession
// which resolves to ManagedSession.SubagentLinker() — i.e. the real
// *cli.Process type assertion. Tests can return a hand-rolled
// *cli.SubagentLinker seeded via SeedFromHistory and skip the process layer
// entirely.
type AgentEventsHandlers struct {
	router     *session.Router
	nodeAccess NodeAccessor
	linkerFor  func(key string) *cli.SubagentLinker
}

// linkerForSession is the default lookup — ManagedSession → *cli.Process
// → SubagentLinker. Returns nil when the session is missing, dead, or
// backed by a non-claude process type.
func (h *AgentEventsHandlers) linkerForSession(key string) *cli.SubagentLinker {
	if h.linkerFor != nil {
		return h.linkerFor(key)
	}
	sess := h.router.GetSession(key)
	if sess == nil {
		return nil
	}
	return sess.SubagentLinker()
}

func (h *AgentEventsHandlers) handleAgentEvents(w http.ResponseWriter, r *http.Request) {
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

	// Remote node proxy parity with /api/sessions/events. Agent event fetches
	// on a peer node go through the reverse-RPC layer; the remote side runs
	// the same handler locally. If the peer binary predates the feature, it
	// returns "unknown method" and we degrade to 404 so the dashboard UI
	// shows "agent has no recorded internals" instead of a 502.
	if nodeID := q.Get("node"); nodeID != "" && nodeID != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return
		}
		entries, err := nc.FetchEvents(r.Context(), key, after)
		_ = entries
		if err != nil {
			if isUnknownRPCMethodErr(err) {
				http.Error(w, "unknown task", http.StatusNotFound)
				return
			}
			slog.Warn("remote fetch agent_events failed", "node", nodeID, "key", key, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		// Remote peers do not yet expose agent_events fan-out — graceful 404
		// until the cross-node feature lands.
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
		// Linker context not yet installed (projectDir / session_id pending
		// the first live init event) — tell client to retry. The dashboard
		// switchAgentView helper has a bounded retry loop that converts a
		// prolonged 202 into the same toast as 404.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"pending"}`))
		return
	}
	if info.InternalAgentID == "" || info.JSONLPath == "" {
		http.Error(w, "unknown task", http.StatusNotFound)
		return
	}

	reader := cli.NewTranscriptReader(info.JSONLPath)
	entries, err := reader.Read(after, limit)
	if err != nil {
		if os.IsNotExist(err) {
			// CLI may have pruned the jsonl (e.g. /new issued on the parent
			// session). Surface 404 so the UI flips to "no record" toast.
			http.Error(w, "unknown task", http.StatusNotFound)
			return
		}
		slog.Warn("agent_events: transcript read failed", "path", info.JSONLPath, "err", err)
		http.Error(w, "transcript read error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, entries)
}

func (h *AgentEventsHandlers) handleToolResult(w http.ResponseWriter, r *http.Request) {
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
		// Tool-result fetches do not yet cross nodes (the persisted-output
		// file lives on the node that ran the CLI). 404 is the contract
		// until a remote-fetch primitive is added.
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
	root = filepath.Clean(root)
	toolResultsRoot := filepath.Join(root, "tool-results")
	if !strings.HasPrefix(resolved, toolResultsRoot+string(filepath.Separator)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if info.Size() > toolResultMaxBytes {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	f, err := os.Open(resolved)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, f); err != nil {
		slog.Debug("tool_result: copy truncated", "err", err)
	}
}
