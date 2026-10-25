package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// mkProjDir creates a project subdir under root and returns its absolute path.
func mkProjDir(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir
}

// newBindServer builds a Server whose router defaults to a tmp workspace and
// whose allowedRoot pins path validation to that root, so /api/sessions/bind
// can be driven end-to-end and the resulting per-chat override read back via
// router.Workspace. AllowedRoot is set explicitly: leaving it empty makes
// validateWorkspace accept any absolute path, which would hide the
// path-traversal rejection assertions.
func newBindServer(t *testing.T) (*Server, *handlerSet, *session.Router, string) {
	t.Helper()
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	router := session.NewRouter(session.RouterConfig{Workspace: resolved})
	srv, hs := buildServerWithHandlers(ServerOptions{
		Addr:        ":0",
		Router:      router,
		Backend:     "claude",
		AllowedRoot: resolved,
	})
	return srv, hs, router, resolved
}

func postBind(t *testing.T, hs *handlerSet, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/bind", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hs.sendH.handleBind(w, req)
	return w
}

// TestHandleBind_PersistsOverride is the core fix assertion: binding a freshly
// created dashboard:pj session writes a per-chat workspace override so a later
// spawn resolves the project dir instead of falling through to defaultCWD.
func TestHandleBind_PersistsOverride(t *testing.T) {
	_, hs, router, root := newBindServer(t)
	projDir := mkProjDir(t, root)

	key := "dashboard:pj:abc0123456789012:general"
	chatKey := "dashboard:pj:abc0123456789012"

	w := postBind(t, hs, `{"key":"`+key+`","node":"local","workspace":"`+projDir+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (body=%s)", w.Code, w.Body.String())
	}
	if got := router.Workspace(chatKey); got != projDir {
		t.Fatalf("Workspace(%q)=%q want %q — override not persisted", chatKey, got, projDir)
	}
}

// TestHandleBind_InvalidWorkspaceRejected ensures a path outside allowedRoot is
// refused with 400 AND that NO override is written (the chat resolves to the
// default workspace, not the attacker path).
func TestHandleBind_InvalidWorkspaceRejected(t *testing.T) {
	_, hs, router, root := newBindServer(t)
	chatKey := "dashboard:pj:abc0123456789012"
	key := chatKey + ":general"

	w := postBind(t, hs, `{"key":"`+key+`","node":"local","workspace":"/etc"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 for out-of-root workspace (body=%s)", w.Code, w.Body.String())
	}
	if got := router.Workspace(chatKey); got != root {
		t.Fatalf("Workspace(%q)=%q want default %q — a rejected bind must not leak an override", chatKey, got, root)
	}
}

// TestHandleBind_InvalidKeyRejected: a key carrying a C1/bidi control byte is
// refused by ValidateSessionKey before any override is written.
func TestHandleBind_InvalidKeyRejected(t *testing.T) {
	_, hs, _, root := newBindServer(t)
	projDir := mkProjDir(t, root)
	// U+202E (RIGHT-TO-LEFT OVERRIDE) embedded in the key.
	key := "dashboard:pj:abc\u202e0123:general"
	w := postBind(t, hs, `{"key":"`+key+`","node":"local","workspace":"`+projDir+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 for control-char key (body=%s)", w.Code, w.Body.String())
	}
}

// TestHandleBind_EmptyWorkspace: a missing workspace is a 400, not a silent
// no-op that could mask a frontend wiring bug.
func TestHandleBind_EmptyWorkspace(t *testing.T) {
	_, hs, _, _ := newBindServer(t)
	w := postBind(t, hs, `{"key":"dashboard:pj:abc0123456789012:general","node":"local","workspace":""}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 for empty workspace (body=%s)", w.Code, w.Body.String())
	}
}

// TestHandleBind_RemoteNodeNoop: a remote-node bind is acked (200) but writes
// NO override on the local router — remote sessions resolve cwd on their node.
func TestHandleBind_RemoteNodeNoop(t *testing.T) {
	_, hs, router, root := newBindServer(t)
	chatKey := "dashboard:pj:abc0123456789012"
	key := chatKey + ":general"
	// Even with a valid-looking workspace, a remote node must not persist locally.
	projDir := mkProjDir(t, root)
	w := postBind(t, hs, `{"key":"`+key+`","node":"remote1","workspace":"`+projDir+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (remote no-op ack) (body=%s)", w.Code, w.Body.String())
	}
	if got := router.Workspace(chatKey); got != root {
		t.Fatalf("Workspace(%q)=%q want default %q — remote bind must not write a local override", chatKey, got, root)
	}
}

// TestHandleBind_ChatKeyDerivation: the override is stored under the 3-segment
// chat-key prefix (last ":agentID" stripped), matching the handleSend contract.
func TestHandleBind_ChatKeyDerivation(t *testing.T) {
	_, hs, router, root := newBindServer(t)
	projDir := mkProjDir(t, root)
	// 4-segment legacy direct key — chat key is everything before the last ':'.
	key := "dashboard:direct:2026-06-06-120228-1-gaokao:general"
	chatKey := "dashboard:direct:2026-06-06-120228-1-gaokao"

	if w := postBind(t, hs, `{"key":"`+key+`","node":"local","workspace":"`+projDir+`"}`); w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (body=%s)", w.Code, w.Body.String())
	}
	if got := router.Workspace(chatKey); got != projDir {
		t.Fatalf("override stored under wrong key: Workspace(%q)=%q want %q", chatKey, got, projDir)
	}
	// The full 4-segment key must NOT itself be an override key.
	if got := router.Workspace(key); got != root {
		t.Fatalf("4-segment key should resolve to default, got %q", got)
	}
}

// TestHandleBind_EmptyChatKeyPrefix: a key whose only colon is at index 0
// (":agent") must be rejected so the empty-string chat key can never carry an
// override (which would poison every default Workspace lookup).
func TestHandleBind_EmptyChatKeyPrefix(t *testing.T) {
	_, hs, router, root := newBindServer(t)
	projDir := mkProjDir(t, root)
	w := postBind(t, hs, `{"key":":general","node":"local","workspace":"`+projDir+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 for empty chat-key prefix (body=%s)", w.Code, w.Body.String())
	}
	if got := router.Workspace(""); got != root {
		t.Fatalf("empty chat key must not carry an override; Workspace(\"\")=%q want %q", got, root)
	}
}

// TestHandleBind_BadJSON: a malformed body is a 400, no panic.
func TestHandleBind_BadJSON(t *testing.T) {
	_, hs, _, _ := newBindServer(t)
	w := postBind(t, hs, `{not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 for malformed JSON (body=%s)", w.Code, w.Body.String())
	}
}

// TestHandleBind_RouteRegistered confirms POST /api/sessions/bind is wired
// behind auth on the mux (a regression that dropped the route would make the
// frontend eager-bind silently 404).
func TestHandleBind_RouteRegistered(t *testing.T) {
	srv, _, _, root := newBindServer(t)
	projDir := mkProjDir(t, root)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/bind",
		strings.NewReader(`{"key":"dashboard:pj:abc0123456789012:general","node":"local","workspace":"`+projDir+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	// No token configured on this Server, so auth wrapper allows the call and
	// the handler runs — anything other than 404/405 proves the route exists.
	if w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/sessions/bind not registered: status=%d", w.Code)
	}
}

// TestHandleBind_WorkspaceLogGoesThroughSanitizeForLog is a source-level
// contract gate mirroring send_sanitize_contract_test.go. handleBind logs the
// attacker-influenced workspace on the validation-failure branch; that attr
// MUST flow through osutil.SanitizeForLog so a `/valid<CRLF>fake-line` or bidi
// payload cannot forge log entries. This locks the shape so a future edit that
// drops the sanitizer is caught in CI (the runtime path is hard to assert on
// without a buffer handler; the source contract matches the four sibling
// sanitize-sink tests in this package).
func TestHandleBind_WorkspaceLogGoesThroughSanitizeForLog(t *testing.T) {
	t.Parallel()
	_, thisFile, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(thisFile), "dashboard_send.go")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read dashboard_send.go: %v", err)
	}
	src := string(data)

	// Negative: a raw `"workspace", req.Workspace` attr (no sanitizer) must not exist.
	if strings.Contains(src, `"workspace", req.Workspace`) {
		t.Error("handleBind logs req.Workspace raw; route it through osutil.SanitizeForLog to prevent log injection")
	}
	// Positive: the sanitized form must be present.
	if !strings.Contains(src, "osutil.SanitizeForLog(req.Workspace,") {
		t.Error("handleBind must route req.Workspace through osutil.SanitizeForLog in the bind validation-failure log attr")
	}
}
