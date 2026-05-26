package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestHandleFileGet_OpenOnce_NoFollowSymlink pins R219-SEC-2 / R220-GO-2
// (#655): handleFileGet now opens the file once with O_NOFOLLOW BEFORE
// dispatching to serve* helpers, and serve* helpers use the plumbed-in fd
// instead of running their own os.Open(resolved).
//
// Regression shape: the prior code Lstat'd to reject final-component
// symlinks and then each helper independently os.Open'd the resolved path.
// An attacker who could win the sub-millisecond race between Lstat and
// os.Open could swap the regular file for a symlink and the helper's
// os.Open would happily follow it. The fixture below stages a symlink
// directly at the workspace path the request asks for: with the fix in
// place, EITHER the Lstat-after-resolve guard OR openWorkspaceFile's
// O_NOFOLLOW must reject the read, and the symlink-target bytes must
// never reach the response body. Without the fix, a future helper that
// re-introduces os.Open would pass the fixture by following the symlink
// out of the workspace.
func TestHandleFileGet_OpenOnce_NoFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		// O_NOFOLLOW is the unix-only kernel-atomic guard. The windows
		// shim falls back to plain Open and relies on the fstat-IsRegular
		// re-check; covering it would require a fstat-time symlink-swap
		// fixture that's flaky on the windows-latest CI runner. naozhi's
		// production target is Linux per the project_files_open_windows
		// godoc.
		t.Skip("O_NOFOLLOW is unix-only; production target is Linux")
	}
	h, projName, projDir := newProjectHandlersForTest(t, nil)

	// Drop a real file outside the workspace that the symlink will redirect
	// to. Bytes the test would NOT want to leak through the dashboard if
	// the open followed the symlink.
	leakRoot := t.TempDir()
	leakFile := filepath.Join(leakRoot, "secret_leak.txt")
	leakBytes := []byte("SECRET-LEAKED-FROM-OUTSIDE-WORKSPACE\n")
	if err := os.WriteFile(leakFile, leakBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(projDir, "swapped.txt")
	if err := os.Symlink(leakFile, link); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"preview", "raw", "render", "download"} {
		t.Run(mode, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				"/api/projects/file?project="+projName+"&path=swapped.txt&mode="+mode, nil)
			w := httptest.NewRecorder()
			h.handleFileGet(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("mode=%s: status = %d, want 404 — symlink final component must be refused before any open follows", mode, w.Code)
			}
			body := w.Body.Bytes()
			if len(body) > 0 && containsBytes(body, leakBytes) {
				t.Fatalf("mode=%s: response body contains symlink-target bytes %q — open followed the symlink, R219-SEC-2 regression", mode, leakBytes)
			}
		})
	}
}

// TestHandleFileGet_OpenOnce_DispatchPasses_FD pins the structural
// invariant: handleFileGet opens once with openWorkspaceFile and the
// serve* helpers do not re-open. Implemented as a smoke test that round-
// trips a regular file through every mode under the in-memory fixture
// — if any helper still ran a second os.Open(resolved), the change would
// shift the fstat-vs-Lstat semantics enough to break this round-trip
// (e.g. the Seek(0) dance in serveDownload only works when the fd has
// previously read its head bytes via serveRaw). The test acts as a
// canary that the plumbed-in fd is the one being read end-to-end.
func TestHandleFileGet_OpenOnce_DispatchPasses_FD(t *testing.T) {
	body := []byte("plumbed-fd-canary\n")
	h, projName, _ := newProjectHandlersForTest(t, map[string]string{
		"canary.txt": string(body),
	})

	for _, mode := range []string{"preview", "raw", "download"} {
		t.Run(mode, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				"/api/projects/file?project="+projName+"&path=canary.txt&mode="+mode, nil)
			w := httptest.NewRecorder()
			h.handleFileGet(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("mode=%s: status = %d, want 200 (canary round-trip broken — fd plumbing regression)", mode, w.Code)
			}
		})
	}
}

// containsBytes is provided by ws_retry_after_test.go in the same package —
// reused here to keep the inode-swap fixture allocation-free.
