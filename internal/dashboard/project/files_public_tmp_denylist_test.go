package project

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestIsPublicTmpDeniedName_Unit pins the suffix/substring/prefix matchers
// independently of any HTTP plumbing so future edits to the lists are caught
// even when the handler glue would otherwise short-circuit. R20260527122801-SEC-6
// (#1330): the deny-list is the last line of defence when a sensitive /tmp
// artefact is world-readable (e.g. 0o777 ssh-agent socket); regressions here
// silently re-open the disclosure path.
func TestIsPublicTmpDeniedName_Unit(t *testing.T) {
	denied := []string{
		"/tmp/ssh-agent.4567",
		"/tmp/postgres.sock",
		"/tmp/redis.sock",
		"/tmp/nginx.pid",
		"/tmp/core.5678",
		"/tmp/crash.report",
		"/tmp/CRASH.dump", // case-insensitive
		"/tmp/MyAgentSSH", // substring
		// gpg-agent artefacts (R20260602190132-SEC-4)
		"/tmp/S.gpg-agent",
		"/tmp/S.gpg-agent.extra",
		"/tmp/S.gpg-agent.ssh",
		"/tmp/GPG-SOCKET", // case-insensitive gpg substring
		// X11 authority artefacts
		"/tmp/.Xauthority", // mixed case — isPublicTmpDeniedName lowercases
		"/tmp/.xauthority",
		// D-Bus artefacts
		"/tmp/.dbus-keyrings",
		"/tmp/.dbus",
	}
	for _, name := range denied {
		if !isPublicTmpDeniedName(name) {
			t.Errorf("isPublicTmpDeniedName(%q) = false; want true", name)
		}
	}

	allowed := []string{
		"/tmp/log.txt",
		"/tmp/build-output.json",
		"/tmp/notes.md",
		"/tmp/some.config",
		"/tmp/", // edge: empty basename
		"",
	}
	for _, name := range allowed {
		if isPublicTmpDeniedName(name) {
			t.Errorf("isPublicTmpDeniedName(%q) = true; want false", name)
		}
	}
}

// TestIsPublicTmpIrregularType_Unit pins the non-regular-type gate
// (R090031-SEC-7 / #1688) independently of HTTP plumbing. A regular file is
// allowed; sockets / FIFOs / device nodes are refused. We synthesize the
// mode via a fake os.FileInfo so the test is portable and doesn't need to
// mknod a device.
func TestIsPublicTmpIrregularType_Unit(t *testing.T) {
	cases := []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{"regular", 0o644, false},
		{"regular-world-rw", 0o666, false},
		{"socket", 0o666 | os.ModeSocket, true},
		{"named-pipe", 0o666 | os.ModeNamedPipe, true},
		{"device", 0o666 | os.ModeDevice, true},
		{"char-device", 0o666 | os.ModeDevice | os.ModeCharDevice, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isPublicTmpIrregularType(fakeModeInfo{mode: tc.mode})
			if got != tc.want {
				t.Errorf("isPublicTmpIrregularType(mode=%v) = %v; want %v", tc.mode, got, tc.want)
			}
		})
	}
}

// fakeModeInfo is a minimal os.FileInfo exposing only Mode(), enough for
// isPublicTmpIrregularType's mode-bit check.
type fakeModeInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (f fakeModeInfo) Mode() os.FileMode { return f.mode }

// TestHandleFileGet_PublicTmpDeniesUnixSocket creates a real, world-readable
// Unix socket under /tmp whose name matches NO deny-list entry. Such a file
// passes both isPublicTmpDeniedName and isPublicTmpForeignPrivate, so only the
// R090031-SEC-7 (#1688) type gate can refuse it. Both HandleFileGet and the
// HandleFilesExists batch probe must hide it.
func TestHandleFileGet_PublicTmpDeniesUnixSocket(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	h.publicTmpEnabled = true

	// Benign name so the name deny-list does NOT trip (no .sock / ssh / etc).
	dir, err := os.MkdirTemp("/tmp", "naozhi-sockgate-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "agent-ipc")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("cannot create unix socket: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// World-readable so the foreign-private gate would NOT block.
	if err := os.Chmod(sockPath, 0o666); err != nil {
		t.Fatal(err)
	}

	// Sanity: name gate alone would let it through.
	if isPublicTmpDeniedName(sockPath) {
		t.Fatalf("test precondition broken: %q matched name deny-list", sockPath)
	}

	rel, err := filepath.Rel("/tmp", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	// HandleFileGet must 404.
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+publicTmpProject+"&path="+rel+"&mode=preview", nil)
	w := httptest.NewRecorder()
	h.HandleFileGet(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unix socket must 404, got %d body=%s", w.Code, w.Body.String())
	}

	// HandleFilesExists batch probe must hide it too.
	body, _ := json.Marshal(existsReq{
		Project: publicTmpProject,
		Paths:   []string{rel},
	})
	pr := httptest.NewRequest(http.MethodPost, "/api/projects/files/exists",
		bytes.NewReader(body))
	pr.Header.Set("Content-Type", "application/json")
	pw := httptest.NewRecorder()
	h.HandleFilesExists(pw, pr)
	if pw.Code != http.StatusOK {
		t.Fatalf("exists status = %d body=%s", pw.Code, pw.Body.String())
	}
	var resp struct {
		Results map[string]existsEntry `json:"results"`
	}
	if err := json.Unmarshal(pw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Results[rel].Exists {
		t.Errorf("unix socket must NOT be reported as existing")
	}
}

// TestHandleFileGet_PublicTmpDeniesSensitiveNames simulates a world-readable
// (0o644) file under /tmp whose name matches the deny-list. The
// foreign-private gate would let it through; the name gate must still 404.
func TestHandleFileGet_PublicTmpDeniesSensitiveNames(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	h.publicTmpEnabled = true

	cases := []struct {
		name    string
		pattern string
	}{
		{"unix-socket", "naozhi-denylist-*.sock"},
		{"pid-file", "naozhi-denylist-*.pid"},
		{"core-dump-name", "core.naozhi-denylist-*"},
		{"ssh-agent-name", "ssh-naozhi-denylist-*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp("/tmp", tc.pattern)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString("sensitive\n")
			f.Close()
			t.Cleanup(func() { _ = os.Remove(f.Name()) })
			// 0o644: world-readable so the foreign-UID gate would NOT block.
			if err := os.Chmod(f.Name(), 0o644); err != nil {
				t.Fatal(err)
			}

			rel, err := filepath.Rel("/tmp", f.Name())
			if err != nil {
				t.Fatal(err)
			}

			// HandleFileGet must 404.
			req := httptest.NewRequest(http.MethodGet,
				"/api/projects/file?project="+publicTmpProject+"&path="+rel+"&mode=preview", nil)
			w := httptest.NewRecorder()
			h.HandleFileGet(w, req)
			if w.Code != http.StatusNotFound {
				t.Errorf("name-denied file must 404, got %d body=%s", w.Code, w.Body.String())
			}

			// HandleFilesExists batch probe must hide it too.
			body, _ := json.Marshal(existsReq{
				Project: publicTmpProject,
				Paths:   []string{rel},
			})
			pr := httptest.NewRequest(http.MethodPost, "/api/projects/files/exists",
				bytes.NewReader(body))
			pr.Header.Set("Content-Type", "application/json")
			pw := httptest.NewRecorder()
			h.HandleFilesExists(pw, pr)
			if pw.Code != http.StatusOK {
				t.Fatalf("exists status = %d body=%s", pw.Code, pw.Body.String())
			}
			var resp struct {
				Results map[string]existsEntry `json:"results"`
			}
			if err := json.Unmarshal(pw.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp.Results[rel].Exists {
				t.Errorf("name-denied file must NOT be reported as existing")
			}
		})
	}
}
