package accessprofile

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/session"
)

// Handler serves the /api/access-profiles endpoint pair.
type Handler struct {
	router Router
	// configPath "" disables the create endpoint (400).
	configPath string
	secretsDir string
	// writeMu serializes the read-modify-write of config.yaml in HandleCreate:
	// two concurrent creates would otherwise both read the same snapshot and
	// the second write would drop the first profile from disk while the live
	// registry kept both (#2360).
	writeMu sync.Mutex
}

// New returns a Handler; configPath "" disables create, secretsDir "" disables token files.
func New(router Router, configPath, secretsDir string) *Handler {
	return &Handler{router: router, configPath: configPath, secretsDir: secretsDir}
}

// HandleList serves the read-only access-profile registry, shaped like
// /api/cli/backends: {"profiles":[{id,display_name,chip_color,default_model,
// default_backend,secret_ok}], "default":""}. SECURITY: only non-sensitive
// metadata; env values and *_FILE contents never leave the server.
func (h *Handler) HandleList(w http.ResponseWriter, _ *http.Request) {
	profiles := h.router.AccessProfileInfos()
	if profiles == nil {
		profiles = []session.AccessProfileInfo{}
	}
	// `default` pre-selects the picker; only a profile ID leaves the server.
	httputil.WriteJSON(w, map[string]any{
		"profiles": profiles,
		"default":  h.router.DefaultAccessProfile(),
	})
}

// createReq is the POST /api/access-profiles body. TokenContent, when set, is
// written to a *_FILE under the secrets dir and referenced via TokenEnvKey.
type createReq struct {
	ID             string            `json:"id"`
	DisplayName    string            `json:"display_name"`
	ChipColor      string            `json:"chip_color"`
	DefaultModel   string            `json:"default_model"`
	DefaultBackend string            `json:"default_backend"`
	Env            map[string]string `json:"env"`
	// TokenEnvKey MUST be a *_FILE indirection key (ANTHROPIC_AUTH_TOKEN_FILE /
	// ANTHROPIC_API_KEY_FILE): the secret lands in a 0600 file, never in
	// config.yaml or later reads.
	TokenEnvKey  string `json:"token_env_key"`
	TokenContent string `json:"token_content"`
}

// HandleCreate creates a new access profile at runtime. Ordered so disk is
// durable before the live registry changes: gate (enabled, id well-formed and
// free) → write token file → validate env → append to config.yaml (atomic) →
// register in the live Router. A failure at any step returns before the next,
// so a profile never half-lands. Token content is NEVER logged or echoed.
func (h *Handler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	if h.configPath == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "access profile creation is disabled (no config path)"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req createReq
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if err := config.ValidateAccessProfileID(req.ID); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid profile id"})
		return
	}

	// HasAccessProfile→append→AddAccessProfile is one critical section (writeMu).
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	if h.router.HasAccessProfile(req.ID) {
		httputil.WriteJSONStatus(w, http.StatusConflict, map[string]string{"error": "profile id already exists"})
		return
	}

	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}

	// secretWritten tracks the token file so a later failure can remove it,
	// leaving no orphaned credential on disk.
	secretWritten := ""
	if req.TokenContent != "" {
		if h.secretsDir == "" {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "secret storage is not configured on this server"})
			return
		}
		// Only a recognised *_FILE indirection key may carry a token path.
		if _, ok := envpolicy.ResolvedFileKey(req.TokenEnvKey); !ok {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "token_env_key must be a *_FILE key (ANTHROPIC_AUTH_TOKEN_FILE / ANTHROPIC_API_KEY_FILE)"})
			return
		}
		// Path derived from the charset-validated id — cannot escape the dir.
		secretPath := filepath.Join(h.secretsDir, req.ID+".token")
		if err := config.WriteSecretFile(secretPath, req.TokenContent); err != nil {
			slog.Error("access profile: write secret failed", "id", req.ID, "err", err)
			httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "failed to write secret file"})
			return
		}
		secretWritten = secretPath
		env[req.TokenEnvKey] = secretPath
	}

	ap := config.AccessProfile{
		DisplayName:    strings.TrimSpace(req.DisplayName),
		ChipColor:      strings.TrimSpace(req.ChipColor),
		DefaultModel:   strings.TrimSpace(req.DefaultModel),
		DefaultBackend: strings.TrimSpace(req.DefaultBackend),
		Env:            env,
	}
	if ap.DefaultBackend != "" && !h.backendEnabled(ap.DefaultBackend) {
		h.cleanupOrphanSecret(secretWritten)
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "default_backend is not an enabled backend"})
		return
	}

	// Persist to config.yaml FIRST so a failure leaves the live registry
	// untouched (disk and memory stay in sync); remove the orphaned secret.
	if err := config.AppendAccessProfile(h.configPath, req.ID, ap); err != nil {
		slog.Warn("access profile: append to config failed", "id", req.ID, "err", err)
		h.cleanupOrphanSecret(secretWritten)
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid profile: " + err.Error()})
		return
	}

	// Register live so the profile works without a restart; AddAccessProfile
	// re-checks for duplicates defensively.
	if err := h.router.AddAccessProfile(req.ID, session.AccessProfile{
		DisplayName:    ap.DisplayName,
		ChipColor:      ap.ChipColor,
		DefaultModel:   ap.DefaultModel,
		DefaultBackend: ap.DefaultBackend,
		Env:            ap.Env,
	}); err != nil {
		// config.yaml already has it; a restart picks it up. Soft error.
		slog.Error("access profile: live registry add failed after config write", "id", req.ID, "err", err)
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "profile saved to config but live registration failed; restart to activate"})
		return
	}

	slog.Info("access profile created", "id", req.ID, "has_secret", req.TokenContent != "")
	httputil.WriteJSON(w, map[string]any{"ok": true, "id": req.ID})
}

// cleanupOrphanSecret best-effort removes a token file written before a later
// create step failed. Empty path is a no-op; removal errors are logged, not
// surfaced (the operator-facing error is the original failure).
func (h *Handler) cleanupOrphanSecret(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("access profile: failed to clean up orphaned secret file after create error", "err", err)
	}
}

// backendEnabled reports whether id is one of the router's enabled backends.
func (h *Handler) backendEnabled(id string) bool {
	for _, b := range h.router.BackendIDs() {
		if b == id {
			return true
		}
	}
	return false
}
