package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/session"
)

// ScratchHandler serves the /api/scratch/* endpoints used by the dashboard
// "aside" drawer: a preview-pane chat seeded with quoted context from the
// main transcript, kept out of the sidebar, and torn down on close or TTL.
type ScratchHandler struct {
	hub       *Hub
	pool      *session.ScratchPool
	openLimit *ipLimiter
	agents    map[string]session.AgentOpts
}

// openRequest is the POST /api/scratch/open body.
type openRequest struct {
	SourceKey       string `json:"source_key"`
	SourceMessageID string `json:"source_message_id,omitempty"` // echoed back for UI jump-to-source; not forwarded to CLI
	Quote           string `json:"quote"`
}

type openResponse struct {
	ScratchID       string `json:"scratch_id"`
	Key             string `json:"key"`
	AgentID         string `json:"agent_id"`
	Backend         string `json:"backend,omitempty"`
	Workspace       string `json:"workspace,omitempty"`
	QuoteTruncated  bool   `json:"quote_truncated,omitempty"`
	SourceMessageID string `json:"source_message_id,omitempty"`
}

// handleOpen creates a scratch session seeded with the quote.
//
// Auth is inherited from the router mux (all /api/scratch/* live behind
// requireAuth). A per-IP limiter throttles creation so a script on an
// authenticated session cannot exhaust the scratch pool or the CLI process
// budget via a tight loop.
func (h *ScratchHandler) handleOpen(w http.ResponseWriter, r *http.Request) {
	if h.openLimit != nil && !h.openLimit.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "open rate limit exceeded"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB — headroom over 8 KiB quote cap
	var req openRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Debug("scratch open: invalid JSON", "err", err)
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Quote == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "quote is required"})
		return
	}
	// Validate the source key at the trust boundary before it is indexed into
	// logs or fed to GetSession — mirrors the IM ValidateSessionKey gate.
	if err := session.ValidateSessionKey(req.SourceKey); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid source_key"})
		return
	}
	// Source session must exist. Without this the pool happily spawns a
	// scratch whose agent/workspace inheritance is based on lookups that
	// silently miss; the user sees a confused "what was I quoting?" aside.
	src := h.hub.router.GetSession(req.SourceKey)
	if src == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "source session not found"})
		return
	}

	// Scratches must not be opened against another scratch (stacking asides
	// would quickly saturate the pool and serves no product need).
	if session.IsScratchKey(req.SourceKey) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "cannot open scratch from another scratch"})
		return
	}

	snap := src.Snapshot()
	agentID := snap.Agent
	if agentID == "" {
		agentID = "general"
	}
	base := session.AgentOpts{}
	if h.agents != nil {
		base = h.agents[agentID]
	}
	// Inherit per-session backend override the source was using (dashboard
	// "pick backend" flow). snap.Backend is empty when the source is using
	// the router default; leaving BaseOpts.Backend empty lets the router
	// fall back to the same default.
	backend := snap.Backend
	workspace := snap.Workspace

	sc, err := h.pool.Open(session.OpenOptions{
		SourceKey: req.SourceKey,
		AgentID:   agentID,
		Backend:   backend,
		Workspace: workspace,
		BaseOpts:  base,
		Quote:     req.Quote,
	})
	if err != nil {
		switch {
		case errors.Is(err, session.ErrQuoteEmpty):
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "quote is empty after sanitization"})
		case errors.Is(err, session.ErrScratchPoolFull):
			writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "scratch pool full"})
		default:
			slog.Warn("scratch open failed", "err", err, "source_key", session.SanitizeLogAttr(req.SourceKey))
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "failed to open scratch"})
		}
		return
	}
	slog.Info("scratch opened", "id", sc.ID, "source", session.SanitizeLogAttr(req.SourceKey), "agent", session.SanitizeLogAttr(agentID), "truncated", sc.QuoteTrunc)
	writeJSON(w, openResponse{
		ScratchID:       sc.ID,
		Key:             sc.Key,
		AgentID:         agentID,
		Backend:         backend,
		Workspace:       workspace,
		QuoteTruncated:  sc.QuoteTrunc,
		SourceMessageID: req.SourceMessageID,
	})
}

// handleDelete tears down a scratch by ID. Idempotent — unknown IDs return
// 204 so a client retry after the TTL sweeper already killed the scratch
// does not surface as an error in the UI.
func (h *ScratchHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidScratchID(id) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid scratch id"})
		return
	}
	if err := h.pool.Close(id); err != nil && !errors.Is(err, session.ErrScratchNotFound) {
		slog.Warn("scratch close failed", "id", id, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// promoteResponse is the JSON body of POST /api/scratch/{id}/promote.
type promoteResponse struct {
	Key string `json:"key"`
}

// handlePromote converts a live scratch into a regular session: the running
// CLI process gets adopted under a new session key (4-segment, visible in
// the sidebar) and the scratch metadata is detached from the pool without
// killing the process. The UI replaces the drawer with the new session.
//
// Ordering rationale (H1): Detach first, THEN RenameSession. Between a bare
// Get and the later Detach, the pool sweeper could independently fire and
// call router.Remove(sc.Key) on the process we're about to promote —
// killing the CLI underneath a user who just clicked "save". Detaching
// first removes the scratch from the sweep set atomically. If the rename
// then fails (collision, validation) we still own the process on sc.Key
// and must clean it up manually via router.Remove to avoid an orphan.
func (h *ScratchHandler) handlePromote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidScratchID(id) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid scratch id"})
		return
	}
	// Detach first so the sweeper's (pool mu → router mu) path cannot race
	// our (router mu via Rename) path. After this point the scratch is
	// entirely our responsibility until RenameSession or Remove lands.
	sc, err := h.pool.Detach(id)
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "scratch not found"})
		return
	}
	// Build the promoted key from the source session key so the UX ties back
	// to the originating chat. Shape:
	//   "{platform}:{chatType}:{chatID}:aside-{agentID}-{shortID}"
	// — still 4 segments, still passes ValidateSessionKey. The agent suffix
	// lets the sidebar show which agent flavour the aside inherited.
	srcParts := strings.SplitN(sc.SourceKey, ":", 4)
	if len(srcParts) != 4 {
		// Shouldn't happen: open-time ValidateSessionKey + the 4-split guard
		// in handleOpen already reject malformed sources. Treat as a
		// defensive programming error, kill the orphan, and report.
		h.hub.router.Remove(sc.Key)
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "source key malformed"})
		return
	}
	short, err := shortPromoteSuffix()
	if err != nil {
		slog.Warn("promote suffix generation failed", "err", err)
		h.hub.router.Remove(sc.Key)
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "failed to promote"})
		return
	}
	newAgent := "aside-" + short
	if sc.AgentID != "" {
		newAgent = "aside-" + sc.AgentID + "-" + short
	}
	newKey := session.SessionKey(srcParts[0], srcParts[1], srcParts[2], newAgent)

	if !h.hub.router.RenameSession(sc.Key, newKey) {
		// Rename failed (collision, invalid new key, or the scratch's
		// session entry vanished between Detach and Rename — the last
		// case shouldn't happen post-Detach but handling it keeps us
		// orphan-free under any future refactor that changes visibility).
		h.hub.router.Remove(sc.Key)
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": "scratch unavailable"})
		return
	}
	h.hub.BroadcastSessionsUpdate()
	slog.Info("scratch promoted", "id", id, "new_key", newKey)
	writeJSON(w, promoteResponse{Key: newKey})
}

// isValidScratchID checks that id is a 32-char lowercase hex string —
// the shape produced by newScratchID. A tight validator here keeps
// operator-controllable path segments out of log attrs / router lookups.
func isValidScratchID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// shortPromoteSuffix returns an 8-char lowercase hex string for use as the
// "aside-<x>" tail on promoted session keys. 32 bits of entropy is enough
// because collisions only need to be avoided within a single chat's agent
// namespace (RenameSession rejects collisions anyway).
func shortPromoteSuffix() (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
