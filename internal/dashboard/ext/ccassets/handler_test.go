package ccassets

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/assets"
	provider "github.com/naozhi/naozhi/internal/ccassets"
)

// allowAll is an IPLimiter that never rate-limits.
type allowAll struct{}

func (allowAll) Allow(string) bool               { return true }
func (allowAll) AllowRequest(*http.Request) bool { return true }

// denyAll always rate-limits.
type denyAll struct{}

func (denyAll) Allow(string) bool               { return false }
func (denyAll) AllowRequest(*http.Request) bool { return false }

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestHandler(t *testing.T, home, repo string, lim IPLimiter) *Handler {
	t.Helper()
	providers := map[string]assets.Provider{"claude": provider.NewClaudeProvider()}
	return New(providers, home, func(*http.Request) string { return repo }, lim)
}

func TestHandleList_OK(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeSkill(t, filepath.Join(home, "skills"), "learned", "---\nname: learned\ndescription: x\n---\nb")
	writeSkill(t, filepath.Join(repo, ".claude", "skills"), "dev-workflow", "---\nname: dev-workflow\ndescription: y\n---\nb")

	h := newTestHandler(t, home, repo, allowAll{})
	rec := httptest.NewRecorder()
	h.HandleList(rec, httptest.NewRequest(http.MethodGet, "/api/cc/assets", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var inv assets.Inventory
	if err := json.Unmarshal(rec.Body.Bytes(), &inv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(inv.Assets) != 2 {
		t.Errorf("assets = %d, want 2", len(inv.Assets))
	}
	if inv.Totals["skill"] != 2 {
		t.Errorf("totals[skill] = %d, want 2", inv.Totals["skill"])
	}
	// No absolute-path leak: the JSON must not contain the temp dir path.
	if body := rec.Body.String(); containsStr(body, home) || containsStr(body, repo) {
		t.Errorf("response leaked an absolute path: %s", body)
	}
}

func TestHandleList_KindFilter(t *testing.T) {
	home := t.TempDir()
	writeSkill(t, filepath.Join(home, "skills"), "learned", "---\nname: learned\n---\nb")

	h := newTestHandler(t, home, "", allowAll{})
	rec := httptest.NewRecorder()
	h.HandleList(rec, httptest.NewRequest(http.MethodGet, "/api/cc/assets?kind=agent", nil))

	var inv assets.Inventory
	_ = json.Unmarshal(rec.Body.Bytes(), &inv)
	if len(inv.Assets) != 0 {
		t.Errorf("kind=agent should yield 0 skill assets, got %d", len(inv.Assets))
	}
	// Totals still reflects the full scan (D4/D5).
	if inv.Totals["skill"] != 1 {
		t.Errorf("totals[skill] = %d, want 1 (full-scan aggregate)", inv.Totals["skill"])
	}
}

func TestHandleList_RateLimited(t *testing.T) {
	h := newTestHandler(t, t.TempDir(), "", denyAll{})
	rec := httptest.NewRecorder()
	h.HandleList(rec, httptest.NewRequest(http.MethodGet, "/api/cc/assets", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
}

func TestHandleRaw_OK(t *testing.T) {
	home := t.TempDir()
	writeSkill(t, filepath.Join(home, "skills"), "learned", "---\nname: learned\n---\nhello world body")

	h := newTestHandler(t, home, "", allowAll{})
	rec := httptest.NewRecorder()
	h.HandleRaw(rec, httptest.NewRequest(http.MethodGet,
		"/api/cc/assets/raw?kind=skill&source=user&rel=skills/learned/SKILL.md", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !containsStr(rec.Body.String(), "hello world body") {
		t.Errorf("raw body missing content: %s", rec.Body.String())
	}
}

func TestHandleRaw_Traversal404(t *testing.T) {
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "secret.txt"), []byte("SECRET"), 0o644)
	writeSkill(t, filepath.Join(home, "skills"), "learned", "---\nname: learned\n---\nb")

	h := newTestHandler(t, home, "", allowAll{})
	rec := httptest.NewRecorder()
	h.HandleRaw(rec, httptest.NewRequest(http.MethodGet,
		"/api/cc/assets/raw?kind=skill&source=user&rel=skills/../../secret.txt", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if containsStr(rec.Body.String(), "SECRET") {
		t.Fatal("traversal leaked secret content")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}
