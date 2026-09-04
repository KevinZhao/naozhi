package scratch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// Handler serves the /api/scratch/* endpoints behind the dashboard "aside"
// drawer: a preview-pane chat seeded with quoted context, kept out of the
// sidebar and torn down on close or TTL. router is the consumer-side
// ScratchRouter view of *session.Router; tests inject a stub (#566).
type Handler struct {
	broadcaster Broadcaster
	router      ScratchRouter
	pool        *session.ScratchPool
	openLimit   IPLimiter
	agents      map[string]session.AgentOpts
}

// openRequest is the POST /api/scratch/open body.
type openRequest struct {
	SourceKey         string `json:"source_key"`
	SourceMessageID   string `json:"source_message_id,omitempty"`   // echoed back for UI jump-to-source; not forwarded to CLI
	SourceMessageTime int64  `json:"source_message_time,omitempty"` // unix ms of the quoted message, used to window surrounding turns
	Quote             string `json:"quote"`
	ContextTurns      int    `json:"context_turns,omitempty"` // requested turn count; 0 = server default, clamped to MaxScratchContextTurns
}

type openResponse struct {
	ScratchID        string `json:"scratch_id"`
	Key              string `json:"key"`
	AgentID          string `json:"agent_id"`
	Backend          string `json:"backend,omitempty"`
	Workspace        string `json:"workspace,omitempty"`
	QuoteTruncated   bool   `json:"quote_truncated,omitempty"`
	ContextTurns     int    `json:"context_turns,omitempty"`     // number of surrounding turns actually injected
	ContextTruncated bool   `json:"context_truncated,omitempty"` // true when some eligible turns did not fit the byte budget
	SourceMessageID  string `json:"source_message_id,omitempty"`
}

// HandleOpen creates a scratch session seeded with the quote. Auth is
// inherited from the router mux; a per-IP limiter stops an authenticated
// script from exhausting the scratch pool or CLI process budget.
func (h *Handler) HandleOpen(w http.ResponseWriter, r *http.Request) {
	if h.openLimit != nil && !h.openLimit.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "open rate limit exceeded"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req openRequest
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		slog.Debug("scratch open: invalid JSON", "err", err)
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Quote == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "quote is required"})
		return
	}
	// The pool sanitizer collapses empty quotes but does not reject bidi/C1
	// runes. Quote becomes a synthetic assistant turn and is echoed into log
	// attrs; scrub at the trust boundary like last_prompt / cron_prompt.
	if !utf8.ValidString(req.Quote) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "quote contains invalid characters"})
		return
	}
	for _, ch := range req.Quote {
		if osutil.IsLogInjectionRune(ch) {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "quote contains invalid characters"})
			return
		}
	}
	// Validate source key at the trust boundary (mirrors the IM ValidateSessionKey gate).
	if err := session.ValidateSessionKey(req.SourceKey); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid source_key"})
		return
	}
	// Source must exist, else inheritance lookups silently miss.
	src := h.router.SessionFor(req.SourceKey)
	if src == nil {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "source session not found"})
		return
	}

	// No scratch-on-scratch: stacking asides would saturate the pool.
	if sessionkey.IsScratchKey(req.SourceKey) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "cannot open scratch from another scratch"})
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
	base = inheritSourceTuning(base, snap)
	// Inherit the source's per-session backend override; empty means router
	// default, and leaving BaseOpts.Backend empty preserves that.
	backend := snap.Backend
	workspace := snap.Workspace

	// ContextTurns is per side; the renderer enforces the byte budget later.
	turns := req.ContextTurns
	if turns <= 0 {
		turns = session.DefaultScratchContextTurns
	}
	if turns > session.MaxScratchContextTurns {
		turns = session.MaxScratchContextTurns
	}
	// No timestamp: best effort, last N entries as before-window, after empty.
	before, after := collectScratchContext(r.Context(), src, req.SourceMessageTime, turns)

	sc, err := h.pool.Open(session.OpenOptions{
		SourceKey:     req.SourceKey,
		AgentID:       agentID,
		Backend:       backend,
		Workspace:     workspace,
		BaseOpts:      base,
		Quote:         req.Quote,
		ContextBefore: before,
		ContextAfter:  after,
	})
	if err != nil {
		switch {
		case errors.Is(err, session.ErrQuoteEmpty):
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "quote is empty after sanitization"})
		case errors.Is(err, session.ErrScratchPoolFull):
			httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "scratch pool full"})
		default:
			slog.Warn("scratch open failed", "err", err, "source_key", session.SanitizeLogAttr(req.SourceKey))
			httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "failed to open scratch"})
		}
		return
	}
	slog.Info("scratch opened",
		"id", sc.ID,
		"source", session.SanitizeLogAttr(req.SourceKey),
		"agent", session.SanitizeLogAttr(agentID),
		"quote_truncated", sc.QuoteTrunc,
		"requested_turns", req.ContextTurns, // pre-clamp, as the client asked
		"applied_turns", turns, // post-clamp, what collectScratchContext used
		"context_turns", sc.ContextTurns, // post-filter + budget, actually rendered
		"context_truncated", sc.ContextTrunc,
	)
	httputil.WriteJSON(w, openResponse{
		ScratchID:        sc.ID,
		Key:              sc.Key,
		AgentID:          agentID,
		Backend:          backend,
		Workspace:        workspace,
		QuoteTruncated:   sc.QuoteTrunc,
		ContextTurns:     sc.ContextTurns,
		ContextTruncated: sc.ContextTrunc,
		SourceMessageID:  req.SourceMessageID,
	})
}

// inheritSourceTuning layers the source session's spawn-time identity
// (access profile, model, effort) onto the agent-registry defaults so the
// aside runs with the same settings as the conversation it quotes (#2433).
//
// Snapshot values are CLI-reported, not operator input: the init frame can
// echo a context-window suffix ("…[1m]") the router's model gate rejects, so
// Model is pre-flighted with session.ValidateModelID (the same gate
// GetOrCreate applies) and Effort with tuningspec. A failing value is
// skipped at Info and the registry default kept. AccessProfile needs no
// gate: an unknown ID degrades to the global default at spawn.
func inheritSourceTuning(base session.AgentOpts, snap session.SessionSnapshot) session.AgentOpts {
	out := base
	if snap.AccessProfile != "" {
		out.AccessProfile = snap.AccessProfile
	}
	if snap.Model != "" {
		if err := session.ValidateModelID(snap.Model); err != nil {
			slog.Info("scratch: not inheriting source model",
				"source_key", session.SanitizeLogAttr(snap.Key),
				"model", session.SanitizeLogAttr(snap.Model), "err", err)
		} else {
			out.Model = snap.Model
		}
	}
	if snap.Effort != "" {
		if err := tuningspec.ValidateEffort("scratch inherited effort", snap.Effort); err != nil {
			slog.Info("scratch: not inheriting source effort",
				"source_key", session.SanitizeLogAttr(snap.Key),
				"effort", session.SanitizeLogAttr(snap.Effort), "err", err)
		} else {
			out.Effort = snap.Effort
		}
	}
	return out
}

// collectScratchContext pulls up to `turns` eligible entries from each side
// of the quoted message (tail of the log when sourceMessageTime == 0). Slices
// stay chronological for the pool's renderer; EventEntriesBeforeCtx reaches
// the disk-tier history when the message is older than the in-memory ring.
func collectScratchContext(ctx context.Context, sess *session.ManagedSession, sourceMessageTime int64, turns int) (before, after []clievent.EventEntry) {
	if sess == nil || turns <= 0 {
		return nil, nil
	}
	// Over-fetch 3x so the pool's filter (drops tool_use / thinking / todo /
	// init / system) still finds `turns` eligible entries; the pool trims.
	fetch := turns * 3
	if sourceMessageTime > 0 {
		before = sess.EventEntriesBeforeCtx(ctx, sourceMessageTime, fetch)
		// SinceInclusive yields Time >= sourceMessageTime; the loop skips the
		// exact match so the quoted message is not echoed into the context.
		raw := sess.EventEntriesSince(cli.SinceInclusive(sourceMessageTime))
		// Cap pre-allocation at `fetch`; the result is fetch-bounded anyway.
		afterCap := len(raw)
		if afterCap > fetch {
			afterCap = fetch
		}
		after = make([]clievent.EventEntry, 0, afterCap)
		for _, e := range raw {
			if e.Time == sourceMessageTime {
				continue
			}
			after = append(after, e)
			if len(after) >= fetch {
				break
			}
		}
	} else {
		// No time hint: tail window only (EventLastN is chronological).
		before = sess.EventLastN(fetch)
	}
	return before, after
}

// HandleDelete tears down a scratch by ID. Idempotent — unknown IDs return
// 204 so a client retry after the TTL sweeper already killed the scratch
// does not surface as an error in the UI.
func (h *Handler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidScratchID(id) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid scratch id"})
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

// HandlePromote converts a live scratch into a regular session: the running
// CLI process is adopted under a new 4-segment key (visible in the sidebar)
// and the scratch metadata is detached from the pool without killing it.
//
// Ordering invariant: Detach first, THEN RenameSession. Otherwise the pool
// sweeper could router.Remove(sc.Key) the process mid-promote. Once detached
// the process is ours: on rename failure we must router.Remove it ourselves.
func (h *Handler) HandlePromote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidScratchID(id) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid scratch id"})
		return
	}
	// Detach first so the sweeper's (pool mu → router mu) path cannot race
	// our Rename path; from here the scratch is our responsibility.
	sc, err := h.pool.Detach(id)
	if err != nil {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "scratch not found"})
		return
	}
	// Promoted key: "{platform}:{chatType}:{chatID}:aside-{agentID}-{shortID}"
	// — still 4 segments, still passes ValidateSessionKey.
	srcParts := strings.SplitN(sc.SourceKey, ":", 4)
	if len(srcParts) != 4 {
		// Unreachable after open-time ValidateSessionKey; kill the orphan and report.
		h.router.Remove(sc.Key)
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "source key malformed"})
		return
	}
	short, err := shortPromoteSuffix()
	if err != nil {
		slog.Warn("promote suffix generation failed", "err", err)
		h.router.Remove(sc.Key)
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "failed to promote"})
		return
	}
	newAgent := "aside-" + short
	if sc.AgentID != "" {
		newAgent = "aside-" + sc.AgentID + "-" + short
	}
	newKey := session.SessionKey(srcParts[0], srcParts[1], srcParts[2], newAgent)

	if !h.router.RenameSession(sc.Key, newKey) {
		// Collision, invalid key, or entry vanished post-Detach: stay orphan-free.
		h.router.Remove(sc.Key)
		httputil.WriteJSONStatus(w, http.StatusConflict, map[string]string{"error": "scratch unavailable"})
		return
	}
	h.broadcaster.BroadcastSessionsUpdate()
	slog.Info("scratch promoted", "id", id, "new_key", newKey)
	httputil.WriteJSON(w, promoteResponse{Key: newKey})
}

// isValidScratchID checks for the 32-char lowercase hex shape newScratchID
// produces, keeping operator-controllable segments out of logs / lookups.
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

// shortPromoteSuffix returns a 16-char lowercase hex string (64 bits of
// entropy, matching other short-id generators) for the "aside-<x>" tail of
// promoted session keys; in-process only, never a stable URL or storage key.
func shortPromoteSuffix() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// Deps bundles all wiring for New so internal/server can construct a Handler
// without access to unexported fields.
type Deps struct {
	Broadcaster Broadcaster
	Router      ScratchRouter
	Pool        *session.ScratchPool
	OpenLimit   IPLimiter
	Agents      map[string]session.AgentOpts
}

// New constructs a Handler from injected deps.
func New(d Deps) *Handler {
	return &Handler{
		broadcaster: d.Broadcaster,
		router:      d.Router,
		pool:        d.Pool,
		openLimit:   d.OpenLimit,
		agents:      d.Agents,
	}
}

// SetOpenLimitForTest lets server-package integration tests bypass the
// open-rate gate. NOT for production use.
func (h *Handler) SetOpenLimitForTest(l IPLimiter) { h.openLimit = l }

// RouterIsWired reports whether the router field has been wired; used by the
// server-package wiring-regression test.
func (h *Handler) RouterIsWired() bool { return h.router != nil }
