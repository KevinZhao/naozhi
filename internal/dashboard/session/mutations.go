package session

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/claudefs"
	sessionpkg "github.com/naozhi/naozhi/internal/session"

	"github.com/naozhi/naozhi/internal/dashboard/contracts"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// HandleDelete serves DELETE /api/sessions. The key comes from the query
// string (?key=&node=, REST-idiomatic, wins when present) or the legacy JSON
// body {key, node} used by the dashboard frontend; both converge on the same
// validation + routing.
func (h *Handlers) HandleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string `json:"key"`
		Node string `json:"node"`
	}
	if q := r.URL.Query(); q.Get("key") != "" {
		req.Key = q.Get("key")
		req.Node = q.Get("node")
		// Drain + close body; MaxBytesReader still bounds a trailer-bomb.
		r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
		if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" {
			http.Error(w, "key is required (pass ?key=... or JSON body)", http.StatusBadRequest)
			return
		}
	}
	// Same gate as HandleEvents: reject multi-KB / control-byte keys before
	// they reach the slog.Warn attr below; also caps at MaxSessionKeyBytes.
	if err := sessionpkg.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	if req.Node != "" && req.Node != "local" {
		nc, ok := h.deps.NodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		removed, err := nc.ProxyRemoveSession(r.Context(), req.Key)
		if err != nil {
			slog.Warn("remote remove session failed", "node", req.Node, "key", req.Key, "err", err)
			if contracts.IsUnknownRPCMethodErr(err) {
				// Peer runs an older binary without remove_session; 409 + explicit
				// body lets the dashboard show "upgrade needed" instead of a
				// generic failure.
				http.Error(w, "remote node needs upgrade to support this action", http.StatusConflict)
				return
			}
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if !removed {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		httputil.WriteOK(w)
		return
	}

	// RemoveAsync: the session leaves the router synchronously (200 truthfully
	// means "gone from the list, accepts no more messages") while the slow
	// teardown (proc.Close up to 8s + socket wait + cleanup) runs detached.
	if !h.deps.Router.RemoveAsync(req.Key) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	httputil.WriteOK(w)
}

// HandleSetLabel serves PATCH /api/sessions/label — the operator-set display
// label for a session. Empty label clears any prior value.
func (h *Handlers) HandleSetLabel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string `json:"key"`
		Node  string `json:"node"`
		Label string `json:"label"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	// Gate req.Key before it reaches slog attrs or router lookups (same policy
	// as HandleEvents / HandleDelete).
	if err := sessionpkg.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}

	label, err := sessionpkg.ValidateUserLabel(req.Label)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Remote node proxy — forward to the node that owns the session.
	if req.Node != "" && req.Node != "local" {
		nc, ok := h.deps.NodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		updated, err := nc.ProxySetSessionLabel(r.Context(), req.Key, label)
		if err != nil {
			// SanitizeLogAttr on node, key AND err.Error(): ValidateSessionKey
			// already rejects bidi / C0 / C1 / zero-width bytes in the key, but
			// req.Node is only validated against the discovery directory and a
			// malicious remote build can echo CR/LF or bidi runes in its RPC error
			// string, fragmenting the local slog audit trail (#820).
			slog.Warn("remote set session label failed",
				"node", sessionpkg.SanitizeLogAttr(req.Node),
				"key", sessionpkg.SanitizeLogAttr(req.Key),
				"err", sessionpkg.SanitizeLogAttr(err.Error()))
			if contracts.IsUnknownRPCMethodErr(err) {
				http.Error(w, "remote node needs upgrade to support this action", http.StatusConflict)
				return
			}
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if !updated {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		// Parallel audit entry with the local-path slog.Info so journalctl shows
		// every label change regardless of owning node; sanitised as above (#820).
		slog.Info("session label updated",
			"node", sessionpkg.SanitizeLogAttr(req.Node),
			"key", sessionpkg.SanitizeLogAttr(req.Key),
			"label_len", len(label))
		// Don't echo label — attacker-influenced text is a latent reflected-XSS
		// vector if a future caller renders the response via innerHTML. Client
		// patches its cache from its own optimistic value.
		httputil.WriteOK(w)
		return
	}

	if !h.deps.Router.SetUserLabel(req.Key, label) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// SanitizeLogAttr on key keeps the audit-log byte class uniform with the
	// remote path (#820).
	slog.Info("session label updated", "node", "local",
		"key", sessionpkg.SanitizeLogAttr(req.Key), "label_len", len(label))
	// Don't echo label — reflected-XSS precaution matches the remote-path
	// above. Client patches its cache from its own optimistic value.
	httputil.WriteOK(w)
}

// POST /api/sessions/resume
func (h *Handlers) HandleResume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID  string `json:"session_id"`
		Workspace  string `json:"workspace"`
		LastPrompt string `json:"last_prompt"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.SessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	if !claudefs.IsValidSessionID(req.SessionID) {
		http.Error(w, "invalid session_id", http.StatusBadRequest)
		return
	}
	// Bound last_prompt: it is persisted and broadcast on every /api/sessions
	// poll, and control chars would inject into slog JSONHandler output.
	if len(req.LastPrompt) > maxResumeLastPromptBytes {
		http.Error(w, "last_prompt too long", http.StatusBadRequest)
		return
	}
	// Invalid UTF-8 is still rejected — a bad encoding usually indicates a
	// buggy client and carries no safe sanitization.
	if !utf8.ValidString(req.LastPrompt) {
		http.Error(w, "last_prompt is not valid utf-8", http.StatusBadRequest)
		return
	}
	// Control / bidi / LS-PS bytes are sanitized to "_" rather than rejected:
	// the slog-injection surface stays closed, yet sessions whose JSONL carries
	// CLI-injected control bytes (e.g. U+0085 NEL from PDF uploads) can still
	// resume from the history pane. last_prompt is display/log-only, so lossy
	// mapping is acceptable; tab is preserved.
	req.LastPrompt = SanitizeResumeLastPrompt(req.LastPrompt, maxResumeLastPromptBytes)

	workspace := req.Workspace
	if workspace != "" {
		var wsPath string
		var err error
		if h.deps.ValidateWS != nil {
			wsPath, err = h.deps.ValidateWS(workspace, h.deps.AllowedRoot)
		}
		if err != nil {
			// Keep the client-facing message decoupled from the error chain so a
			// wrapped *os.PathError can't leak resolved filesystem paths;
			// validateWorkspace already logs detail. Sanitize the workspace before
			// it lands in slog attrs — authenticated callers can slip bidi / C1 /
			// newline bytes past the structural path check.
			slog.Warn("resume workspace validation failed", "err", err, "workspace", osutil.SanitizeForLog(workspace, 256))
			httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "invalid workspace"})
			return
		}
		workspace = wsPath
	}
	if workspace == "" {
		workspace = h.deps.Router.DefaultWorkspace()
	}

	// 16 random bytes (128 bits) so the resume key tail matches anonCookie /
	// upload IDs and the codebase's short-id entropy budget.
	var rb [16]byte
	if _, err := rand.Read(rb[:]); err != nil {
		// crypto/rand failures are pathologically rare; log so operators can
		// distinguish this from other 500s.
		slog.Error("resume register: generate key failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	key := "dashboard:direct:r" + hex.EncodeToString(rb[:]) + ":general"
	effectiveKey := h.deps.Router.RegisterForResume(key, req.SessionID, workspace, req.LastPrompt)

	httputil.WriteJSON(w, map[string]string{"status": "ok", "key": effectiveKey})
}

// POST /api/sessions/interrupt
func (h *Handlers) HandleInterrupt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string `json:"key"`
		Node string `json:"node"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	// Gate req.Key before it reaches slog attrs / router lookup (same policy as
	// the other session handlers).
	if err := sessionpkg.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	if req.Node != "" && req.Node != "local" {
		nc, ok := h.deps.NodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		interrupted, err := nc.ProxyInterruptSession(r.Context(), req.Key)
		if err != nil {
			slog.Warn("remote interrupt session failed", "node", req.Node, "key", req.Key, "err", err)
			if contracts.IsUnknownRPCMethodErr(err) {
				http.Error(w, "remote node needs upgrade to support this action", http.StatusConflict)
				return
			}
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if interrupted {
			slog.Info("remote session interrupted via HTTP", "node", req.Node, "key", req.Key)
			httputil.WriteOK(w)
		} else {
			httputil.WriteJSON(w, map[string]string{"status": "not_running"})
		}
		return
	}

	// Prefer control_request over SIGINT — see Router.InterruptSessionSafe
	// for why raw SIGINT on `-p` mode is destructive.
	switch h.deps.Router.InterruptSessionSafe(req.Key) {
	case sessionpkg.InterruptSent:
		slog.Info("session interrupted via HTTP", "key", req.Key)
		httputil.WriteOK(w)
	case sessionpkg.InterruptNoSession:
		httputil.WriteJSON(w, map[string]string{"status": "not_running"})
	default:
		httputil.WriteJSON(w, map[string]string{"status": "not_running"})
	}
}
