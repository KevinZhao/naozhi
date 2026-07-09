package server

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/session"
)

// createAccessProfileReq is the POST /api/access-profiles body (RFC
// project-access-profile P1-d). The dashboard "create profile" form fills it
// from a template ("个人 Anthropic" / "公司 Bedrock") plus a token. Env keys are
// the literal overlay keys (already-known set); TokenContent, when non-empty,
// is written to a *_FILE under the trusted secrets dir and referenced from env
// via TokenEnvKey.
type createAccessProfileReq struct {
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

// handleCreateAccessProfile creates a new access profile at runtime (P1-d).
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
func (s *Server) handleCreateAccessProfile(w http.ResponseWriter, r *http.Request) {
	if s.configPath == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "access profile creation is disabled (no config path)"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req createAccessProfileReq
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	req.ID = strings.TrimSpace(req.ID)
	if err := config.ValidateAccessProfileID(req.ID); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid profile id"})
		return
	}
	if s.router.HasAccessProfile(req.ID) {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": "profile id already exists"})
		return
	}

	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}

	// Secret file: write token content to a 0600 file the profile references.
	if req.TokenContent != "" {
		if s.accessProfileSecretsDir == "" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "secret storage is not configured on this server"})
			return
		}
		// Only a recognised *_FILE indirection key may carry a token path.
		if _, ok := envpolicy.ResolvedFileKey(req.TokenEnvKey); !ok {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "token_env_key must be a *_FILE key (ANTHROPIC_AUTH_TOKEN_FILE / ANTHROPIC_API_KEY_FILE)"})
			return
		}
		// Path derived from the charset-validated id — cannot escape the dir.
		secretPath := filepath.Join(s.accessProfileSecretsDir, req.ID+".token")
		if err := config.WriteSecretFile(secretPath, req.TokenContent); err != nil {
			slog.Error("access profile: write secret failed", "id", req.ID, "err", err)
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "failed to write secret file"})
			return
		}
		env[req.TokenEnvKey] = secretPath
	}

	ap := config.AccessProfile{
		DisplayName:    strings.TrimSpace(req.DisplayName),
		ChipColor:      strings.TrimSpace(req.ChipColor),
		DefaultModel:   strings.TrimSpace(req.DefaultModel),
		DefaultBackend: strings.TrimSpace(req.DefaultBackend),
		Env:            env,
	}
	if ap.DefaultBackend != "" && !s.backendEnabled(ap.DefaultBackend) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "default_backend is not an enabled backend"})
		return
	}

	// Persist to config.yaml FIRST (validates env + id, atomic write). If this
	// fails the live registry is untouched, so disk and memory stay in sync.
	if err := config.AppendAccessProfile(s.configPath, req.ID, ap); err != nil {
		slog.Warn("access profile: append to config failed", "id", req.ID, "err", err)
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid profile: " + err.Error()})
		return
	}

	// Register live so the new profile works without a restart. Duplicate is
	// impossible here (HasAccessProfile gate above + config append already
	// rejected a dup), but AddAccessProfile re-checks defensively.
	if err := s.router.AddAccessProfile(req.ID, session.AccessProfile{
		DisplayName:    ap.DisplayName,
		ChipColor:      ap.ChipColor,
		DefaultModel:   ap.DefaultModel,
		DefaultBackend: ap.DefaultBackend,
		Env:            ap.Env,
	}); err != nil {
		// config.yaml already has it; a restart would pick it up. Surface a
		// soft error so the operator knows to reload if the live add raced.
		slog.Error("access profile: live registry add failed after config write", "id", req.ID, "err", err)
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "profile saved to config but live registration failed; restart to activate"})
		return
	}

	slog.Info("access profile created", "id", req.ID, "has_secret", req.TokenContent != "")
	writeJSON(w, map[string]any{"ok": true, "id": req.ID})
}

// backendEnabled reports whether id is one of the router's enabled backends.
func (s *Server) backendEnabled(id string) bool {
	for _, b := range s.router.BackendIDs() {
		if b == id {
			return true
		}
	}
	return false
}
