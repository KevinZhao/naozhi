package accessprofile

// edit.go — the settings page's editor: change or delete an access profile
// (config.yaml) and add or remove a model (naozhi's own Claude settings file).
// Every write keeps the keys the editor does not know, backs the old file up
// as <file>.bak-<time> and keeps the file's mode (config.WriteFileWithBackup).

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/naozhi/naozhi/internal/ccmodels"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// updateReq is the PUT /api/access-profiles body: the editable fields of an
// existing profile ("" clears one). Env stays as configured.
type updateReq struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	ChipColor      string `json:"chip_color"`
	DefaultModel   string `json:"default_model"`
	DefaultBackend string `json:"default_backend"`
	AWSProfile     string `json:"aws_profile"`
}

// HandleUpdate edits a profile: config.yaml first, then the live registry, so
// sessions spawned from here on use the new fields.
func (h *Handler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	if h.configPath == "" {
		writeErr(w, http.StatusBadRequest, "access profile editing is disabled (no config path)")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req updateReq
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	f := config.AccessProfileFields{
		DisplayName:    strings.TrimSpace(req.DisplayName),
		ChipColor:      strings.TrimSpace(req.ChipColor),
		DefaultModel:   strings.TrimSpace(req.DefaultModel),
		DefaultBackend: strings.TrimSpace(req.DefaultBackend),
		AWSProfile:     strings.TrimSpace(req.AWSProfile),
	}
	if f.DefaultBackend != "" && !h.backendEnabled(f.DefaultBackend) {
		writeErr(w, http.StatusBadRequest, "default_backend is not an enabled backend")
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	cur, ok := h.router.AccessProfiles()[req.ID]
	if !ok {
		writeErr(w, http.StatusNotFound, "profile not found")
		return
	}
	if err := config.UpdateAccessProfile(h.configPath, req.ID, f); err != nil {
		slog.Warn("access profile: update in config failed", "id", req.ID, "err", err)
		writeErr(w, http.StatusBadRequest, "invalid profile: "+err.Error())
		return
	}
	cur.DisplayName, cur.ChipColor, cur.DefaultModel, cur.DefaultBackend, cur.AWSProfile =
		f.DisplayName, f.ChipColor, f.DefaultModel, f.DefaultBackend, f.AWSProfile
	if err := h.router.ReplaceAccessProfile(req.ID, cur); err != nil {
		slog.Error("access profile: live update failed after config write", "id", req.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "profile saved to config but not applied; restart to apply")
		return
	}
	slog.Info("access profile updated", "id", req.ID, "aws_profile", f.AWSProfile)
	httputil.WriteJSON(w, map[string]any{"ok": true, "id": req.ID})
}

// HandleDelete removes the profile ?id= from config.yaml and the registry.
// Sessions recorded on it run on the global credentials from their next spawn.
func (h *Handler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	if h.configPath == "" {
		writeErr(w, http.StatusBadRequest, "access profile editing is disabled (no config path)")
		return
	}
	id := r.URL.Query().Get("id")
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if !h.router.HasAccessProfile(id) {
		writeErr(w, http.StatusNotFound, "profile not found")
		return
	}
	if err := config.RemoveAccessProfile(h.configPath, id); err != nil {
		slog.Warn("access profile: remove from config failed", "id", id, "err", err)
		writeErr(w, http.StatusBadRequest, "cannot delete: "+err.Error())
		return
	}
	if err := h.router.RemoveAccessProfile(id); err != nil {
		slog.Error("access profile: live remove failed after config write", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "profile removed from config but still live; restart to apply")
		return
	}
	slog.Info("access profile deleted", "id", id)
	httputil.WriteJSON(w, map[string]any{"ok": true})
}

// HandleModels lists the settings file's models: {enabled, path, models}.
func (h *Handler) HandleModels(w http.ResponseWriter, _ *http.Request) {
	if h.settingsPath == "" {
		httputil.WriteJSON(w, map[string]any{"enabled": false, "models": []ccmodels.Model{}})
		return
	}
	doc, err := os.ReadFile(h.settingsPath)
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "cannot read the settings file")
		return
	}
	models, err := ccmodels.ListModels(doc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(w, map[string]any{"enabled": true, "path": h.settingsPath, "models": models})
}

// addModelReq is the POST /api/settings/models body.
type addModelReq struct {
	Model     string `json:"model"`
	Label     string `json:"label"`
	BehavesAs string `json:"behaves_as"`
}

// HandleAddModel adds a model to the settings file (ccmodels.AddModel). New
// sessions offer it at once: the CLI reads the file at spawn.
func (h *Handler) HandleAddModel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req addModelReq
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	h.editSettings(w, func(doc []byte) ([]byte, error) {
		return ccmodels.AddModel(doc, req.Model, strings.TrimSpace(req.Label), strings.TrimSpace(req.BehavesAs))
	})
}

// HandleRemoveModel removes ?model= from the settings file.
func (h *Handler) HandleRemoveModel(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	h.editSettings(w, func(doc []byte) ([]byte, error) { return ccmodels.RemoveModel(doc, model) })
}

func (h *Handler) editSettings(w http.ResponseWriter, edit func([]byte) ([]byte, error)) {
	if h.settingsPath == "" {
		writeErr(w, http.StatusBadRequest, "model editing needs naozhi_settings.enabled")
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	doc, err := os.ReadFile(h.settingsPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot read the settings file")
		return
	}
	out, err := edit(doc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.WriteFileWithBackup(h.settingsPath, doc, append(out, '\n')); err != nil {
		slog.Error("model settings: write failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "cannot write the settings file")
		return
	}
	slog.Info("model settings edited", "path", h.settingsPath)
	httputil.WriteJSON(w, map[string]any{"ok": true})
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	httputil.WriteJSONStatus(w, status, map[string]string{"error": msg})
}
