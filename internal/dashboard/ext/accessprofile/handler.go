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
	// configPath / secretsDir enable the create endpoint (RFC
	// project-access-profile P1-d). Empty configPath keeps it
	// disabled (400).
	configPath string
	secretsDir string
	// writeMu serializes the read-modify-write of config.yaml in
	// HandleCreate. AppendAccessProfile snapshots the file, inserts,
	// and atomically rewrites; two concurrent creates would otherwise
	// interleave (both read the same snapshot, second write drops the
	// first profile from config.yaml while the live registry kept both —
	// a silent disk/memory divergence surfacing only on restart).
	// [PR#2360 review HIGH]
	writeMu sync.Mutex
}

// New returns a Handler. configPath "" disables the create endpoint;
// secretsDir "" disables token-file storage within create.
func New(router Router, configPath, secretsDir string) *Handler {
	return &Handler{router: router, configPath: configPath, secretsDir: secretsDir}
}

// HandleList serves the read-only access-profile registry (RFC
// project-access-profile §8.1). Response shape mirrors /api/cli/backends:
//
//	{"profiles":[{id,display_name,chip_color,default_model,default_backend,secret_ok}], "default":""}
//
// SECURITY: the payload carries ONLY non-sensitive metadata. Env values and
// *_FILE contents never leave the server — AccessProfileInfos projects the
// registry down to display fields + a secret_ok preflight bit.
func (h *Handler) HandleList(w http.ResponseWriter, _ *http.Request) {
	profiles := h.router.AccessProfileInfos()
	if profiles == nil {
		profiles = []session.AccessProfileInfo{}
	}
	// `default` carries the configured default_access_profile so the
	// new-session picker can pre-select it (instead of the bare "(global
	// default)" empty option). Empty when no default is configured — the
	// picker then falls back to the empty option as before. Only a
	// non-sensitive profile ID leaves the server here; env/token stay behind
	// AccessProfileInfos' projection.
	httputil.WriteJSON(w, map[string]any{
		"profiles": profiles,
		"default":  h.router.DefaultAccessProfile(),
	})
}

// createReq is the POST /api/access-profiles body (RFC
// project-access-profile P1-d). The dashboard "create profile" form fills it
// from a template ("个人 Anthropic" / "公司 Bedrock") plus a token. Env keys are
// the literal overlay keys (already-known set); TokenContent, when non-empty,
// is written to a *_FILE under the trusted secrets dir and referenced from env
// via TokenEnvKey.
type createReq struct {
	ID             string            `json:"id"`
	DisplayName    string            `json:"display_name"`
	ChipColor      string            `json:"chip_color"`
	DefaultModel   string            `json:"default_model"`
	DefaultBackend string            `json:"default_backend"`
	Env            map[string]string `json:"env"`
	// TokenEnvKey + TokenContent: when both set, the server writes TokenContent
	// to a 0600 file under the secrets dir and injects
	// env[TokenEnvKey] = <that path>. TokenEnvKey MUST be a *_FILE indirection
	// key (ANTHROPIC_AUTH_TOKEN_FILE / ANTHROPIC_API_KEY_FILE). This keeps the
	// secret off the wire in future reads and out of config.yaml.
	TokenEnvKey  string `json:"token_env_key"`
	TokenContent string `json:"token_content"`
}

// HandleCreate creates a new access profile at runtime (P1-d).
// Flow, ordered so disk is durable before the live registry changes:
//  1. gate: feature enabled (configPath set), id well-formed, id not taken;
//  2. if a token is supplied, write it to <secretsDir>/<id>.token (0600) and
//     point the *_FILE env key at it;
//  3. validate every env entry (envpolicy leaf — same as load path);
//  4. append to config.yaml (yaml.Node surgery, atomic, re-validated);
//  5. register in the live Router so it works WITHOUT a restart.
//
// A failure at any step returns before the next, so a rejected profile never
// half-lands (config written but registry not, or vice versa). The token
// content is NEVER logged or echoed back.
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

	// Serialize the whole read-modify-write of config.yaml + live registry.
	// The HasAccessProfile→append→AddAccessProfile trio must be one critical
	// section (see writeMu doc). [PR#2360 review HIGH]
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

	// Secret file: write token content to a 0600 file the profile references.
	// secretWritten tracks the path so it can be removed if a later step fails,
	// leaving no orphaned credential file behind. [PR#2360 review MEDIUM]
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

	// Persist to config.yaml FIRST (validates env + id, atomic write). If this
	// fails the live registry is untouched, so disk and memory stay in sync;
	// the just-written secret file is orphaned, so remove it best-effort.
	if err := config.AppendAccessProfile(h.configPath, req.ID, ap); err != nil {
		slog.Warn("access profile: append to config failed", "id", req.ID, "err", err)
		h.cleanupOrphanSecret(secretWritten)
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid profile: " + err.Error()})
		return
	}

	// Register live so the new profile works without a restart. Duplicate is
	// impossible here (HasAccessProfile gate above + config append already
	// rejected a dup), but AddAccessProfile re-checks defensively.
	if err := h.router.AddAccessProfile(req.ID, session.AccessProfile{
		DisplayName:    ap.DisplayName,
		ChipColor:      ap.ChipColor,
		DefaultModel:   ap.DefaultModel,
		DefaultBackend: ap.DefaultBackend,
		Env:            ap.Env,
	}); err != nil {
		// config.yaml already has it; a restart would pick it up. Surface a
		// soft error so the operator knows to reload if the live add raced.
		slog.Error("access profile: live registry add failed after config write", "id", req.ID, "err", err)
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "profile saved to config but live registration failed; restart to activate"})
		return
	}

	slog.Info("access profile created", "id", req.ID, "has_secret", req.TokenContent != "")
	httputil.WriteJSON(w, map[string]any{"ok": true, "id": req.ID})
}

// cleanupOrphanSecret best-effort removes a token file that was written before
// a later create step failed, so a credential-bearing file is never left on
// disk with no profile referencing it. Empty path is a no-op (no secret was
// written). Removal errors are logged, not surfaced — the operator-facing
// error is the original failure, and a stray 0600 file is not a security
// exposure. [PR#2360 review MEDIUM]
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
