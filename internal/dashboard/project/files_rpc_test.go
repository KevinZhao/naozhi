package project

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
	projectpkg "github.com/naozhi/naozhi/internal/project"
)

const rpcRepoKey = "dashboard:direct:repo:general"

// newNodeHandlers is newSessionRootHandlers with a project "demo" on the
// repo, a text file past the preview cap and a binary file spanning three
// file_read segments; it returns the handlers and the big file's bytes.
func newNodeHandlers(t *testing.T) (*Handlers, []byte) {
	t.Helper()
	h := newSessionRootHandlers(t)
	repo := filepath.Dir(h.deps.Router.SessionFor(rpcRepoKey).Workspace())
	big := bytes.Repeat([]byte("0123456789abcdef"), (2*node.MaxFileChunk+node.MaxFileChunk/2)/16)
	big[0] = 0 // binary: never previewed as text
	if err := os.WriteFile(filepath.Join(repo, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "long.txt"), bytes.Repeat([]byte("a"), maxPreviewBytes+10), 0o644); err != nil {
		t.Fatal(err)
	}
	h.deps.ProjectMgr = &fakeStore{projects: map[string]*projectpkg.Project{"demo": {Name: "demo", Path: repo}}}
	return h, big
}

func TestReadFileFor(t *testing.T) {
	t.Parallel()
	h, big := newNodeHandlers(t)
	repo := node.FileRoot{Key: rpcRepoKey}
	cases := []struct {
		name       string
		p          node.FileReadParams
		status     int
		data       string
		size       int64
		truncated  bool
		binary     bool
		wantLength int
	}{
		{name: "preview", p: node.FileReadParams{FileRoot: repo, Path: "README.md", Mode: "preview"}, data: "hello", size: 5},
		{name: "project root", p: node.FileReadParams{FileRoot: node.FileRoot{Project: "demo"}, Path: "sub/a.txt", Mode: "preview"}, data: "in sub", size: 6},
		{name: "preview first segment", p: node.FileReadParams{FileRoot: repo, Path: "long.txt", Mode: "preview"}, size: maxPreviewBytes + 10, truncated: true, wantLength: node.ReverseFileChunk},
		{name: "preview cap", p: node.FileReadParams{FileRoot: repo, Path: "long.txt", Mode: "preview", Offset: maxPreviewBytes - 100}, size: maxPreviewBytes + 10, truncated: true, wantLength: 100},
		{name: "preview offset past end", p: node.FileReadParams{FileRoot: repo, Path: "README.md", Mode: "preview", Offset: 6}, status: http.StatusBadRequest},
		{name: "binary preview", p: node.FileReadParams{FileRoot: repo, Path: "big.bin", Mode: "preview"}, size: int64(len(big)), binary: true},
		{name: "sensitive preview", p: node.FileReadParams{FileRoot: repo, Path: ".env", Mode: "preview"}, size: 8, binary: true},
		{name: "sensitive raw", p: node.FileReadParams{FileRoot: repo, Path: ".env", Mode: "raw"}, status: http.StatusForbidden},
		{name: "traversal", p: node.FileReadParams{FileRoot: repo, Path: "../../outside/x.txt", Mode: "raw"}, status: http.StatusNotFound},
		{name: "absolute path", p: node.FileReadParams{FileRoot: repo, Path: "/etc/passwd", Mode: "raw"}, status: http.StatusNotFound},
		{name: "symlink file escape", p: node.FileReadParams{FileRoot: repo, Path: "escape", Mode: "preview"}, status: http.StatusNotFound},
		{name: "symlink dir escape", p: node.FileReadParams{FileRoot: repo, Path: "linkdir/x.txt", Mode: "raw"}, status: http.StatusNotFound},
		{name: "outside allowed root", p: node.FileReadParams{FileRoot: node.FileRoot{Key: "dashboard:direct:out:general"}, Path: "x.txt", Mode: "raw"}, status: http.StatusNotFound},
		{name: "unknown session", p: node.FileReadParams{FileRoot: node.FileRoot{Key: "dashboard:direct:nope:general"}, Path: "README.md", Mode: "raw"}, status: http.StatusNotFound},
		{name: "invalid mode", p: node.FileReadParams{FileRoot: repo, Path: "README.md", Mode: "render"}, status: http.StatusBadRequest},
		{name: "first segment", p: node.FileReadParams{FileRoot: repo, Path: "big.bin", Mode: "raw"}, size: int64(len(big)), wantLength: node.ReverseFileChunk},
		{name: "oversized limit clamps", p: node.FileReadParams{FileRoot: repo, Path: "big.bin", Mode: "raw", Limit: node.MaxFileChunk}, size: int64(len(big)), wantLength: node.ReverseFileChunk},
		{name: "last segment", p: node.FileReadParams{FileRoot: repo, Path: "big.bin", Mode: "raw", Offset: int64(len(big) - 100)}, size: int64(len(big)), wantLength: 100},
		{name: "small limit", p: node.FileReadParams{FileRoot: repo, Path: "README.md", Mode: "raw", Offset: 1, Limit: 3}, size: 5, data: "ell"},
		{name: "offset past end", p: node.FileReadParams{FileRoot: repo, Path: "README.md", Mode: "raw", Offset: 6}, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Through the wire as the RPC carries it.
			var got node.FileRead
			raw, _ := json.Marshal(h.ReadFileFor(tc.p))
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.status {
				t.Fatalf("status = %d (%s), want %d", got.Status, got.Error, tc.status)
			}
			if tc.status != 0 {
				return
			}
			if got.Size != tc.size || got.Truncated != tc.truncated || got.Binary != tc.binary {
				t.Errorf("size/truncated/binary = %d/%v/%v, want %d/%v/%v", got.Size, got.Truncated, got.Binary, tc.size, tc.truncated, tc.binary)
			}
			if tc.data != "" && string(got.Data) != tc.data {
				t.Errorf("data = %q, want %q", got.Data, tc.data)
			}
			if tc.wantLength != 0 && len(got.Data) != tc.wantLength {
				t.Errorf("len(data) = %d, want %d", len(got.Data), tc.wantLength)
			}
			if tc.p.Mode == "raw" && tc.wantLength != 0 && !bytes.Equal(got.Data, big[tc.p.Offset:tc.p.Offset+int64(tc.wantLength)]) {
				t.Error("segment bytes differ from the file")
			}
			if tc.binary && len(got.Data) != 0 {
				t.Errorf("binary or sensitive preview carries %d bytes", len(got.Data))
			}
			if len(raw) > node.ReverseFileChunk*4/3+1024 {
				t.Errorf("answer is %d bytes, over one reverse file chunk", len(raw))
			}
		})
	}
}

func TestListFilesFor(t *testing.T) {
	t.Parallel()
	h, _ := newNodeHandlers(t)
	cases := []struct {
		name   string
		p      node.FilesListParams
		status int
		want   []string
	}{
		{name: "session root", p: node.FilesListParams{FileRoot: node.FileRoot{Key: rpcRepoKey}, ShowHidden: true}, want: []string{"README.md", "sub", "big.bin"}},
		{name: "project subdir", p: node.FilesListParams{FileRoot: node.FileRoot{Project: "demo"}, Dir: "sub"}, want: []string{"a.txt"}},
		{name: "traversal", p: node.FilesListParams{FileRoot: node.FileRoot{Key: rpcRepoKey}, Dir: ".."}, status: http.StatusNotFound},
		{name: "symlink dir escape", p: node.FilesListParams{FileRoot: node.FileRoot{Key: rpcRepoKey}, Dir: "linkdir"}, status: http.StatusNotFound},
		{name: "unknown project", p: node.FilesListParams{FileRoot: node.FileRoot{Project: "nope"}}, status: http.StatusNotFound},
		{name: "invalid key", p: node.FilesListParams{FileRoot: node.FileRoot{Key: "a\x01b"}}, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := h.ListFilesFor(tc.p)
			if got.Status != tc.status {
				t.Fatalf("status = %d (%s), want %d", got.Status, got.Error, tc.status)
			}
			names := map[string]bool{}
			for _, e := range got.Entries {
				names[e.Name] = true
			}
			for _, n := range tc.want {
				if !names[n] {
					t.Errorf("%s missing from %v", n, got.Entries)
				}
			}
			if names[".env"] {
				t.Error(".env enumerated")
			}
		})
	}
}

// fakeNodes is a NodeAccessor over fixed conns.
type fakeNodes map[string]node.Conn

func (f fakeNodes) HasNodes() bool                       { return len(f) > 0 }
func (f fakeNodes) NodesSnapshot() map[string]node.Conn  { return f }
func (f fakeNodes) NodeByID(id string) (node.Conn, bool) { c, ok := f[id]; return c, ok }
func (f fakeNodes) KnownNodes() map[string]string        { return nil }
func (f fakeNodes) LookupNode(w http.ResponseWriter, id string) (node.Conn, bool) {
	c, ok := f[id]
	if !ok {
		http.Error(w, "unknown node", http.StatusBadRequest)
	}
	return c, ok
}

// rpcNode stands for a reverse node: its file RPCs reach the node's handlers
// through a JSON round trip, as the wire carries them.
type rpcNode struct {
	node.Conn
	h    *Handlers
	caps map[string]bool
}

func (n rpcNode) Meta() *node.NodeMeta { return &node.NodeMeta{Capabilities: n.caps} }

func roundTrip[T any](v T) (T, error) {
	var out T
	raw, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}

func (n rpcNode) ListFiles(_ context.Context, p node.FilesListParams) (node.FileList, error) {
	return roundTrip(n.h.ListFilesFor(p))
}
func (n rpcNode) ReadFile(_ context.Context, p node.FileReadParams) (node.FileRead, error) {
	return roundTrip(n.h.ReadFileFor(p))
}
func (n rpcNode) FilesExist(_ context.Context, p node.FilesExistsParams) (node.FilesExists, error) {
	return roundTrip(n.h.FilesExistFor(p))
}
func (n rpcNode) FetchOutputs(context.Context, string) (json.RawMessage, error) {
	return nil, node.ErrFilesUnsupported
}

// TestFileAPI_NodeRouting: a node=<id> request reads that node's files —
// over the reverse RPCs or a direct node's HTTP file API — and never this
// host's, though a local project and session carry the same names; a node
// that cannot serve its files answers 501.
func TestFileAPI_NodeRouting(t *testing.T) {
	t.Parallel()
	nodeH, big := newNodeHandlers(t)
	mux := http.NewServeMux()
	for _, rt := range nodeH.Routes() {
		mux.HandleFunc(rt.Pattern, rt.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// The primary: a local project "demo" and a local session under the same
	// key, both holding README.md with other content.
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "README.md"), []byte("LOCAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{
		ProjectMgr:  &fakeStore{projects: map[string]*projectpkg.Project{"demo": {Name: "demo", Path: local}}},
		Router:      &fakeRouter{workspaces: map[string]string{rpcRepoKey: local}},
		AllowedRoot: filepath.Dir(local),
		ValidateWS:  func(ws, _ string) (string, error) { return filepath.EvalSymlinks(ws) },
		NodeAccess: fakeNodes{
			"rev":    rpcNode{h: nodeH, caps: map[string]bool{node.CapFiles: true}},
			"direct": node.NewHTTPClient("direct", srv.URL, "", "direct"),
			"old":    rpcNode{h: nodeH},
		},
	})

	get := func(t *testing.T, path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if strings.HasPrefix(path, "/api/projects/files/list") {
			h.HandleFilesList(w, r)
		} else {
			h.HandleFileGet(w, r)
		}
		return w
	}
	root := "root=" + url.QueryEscape(sessionRootPrefix+rpcRepoKey)
	for _, nodeID := range []string{"rev", "direct"} {
		t.Run(nodeID, func(t *testing.T) {
			q := root + "&node=" + nodeID
			if w := get(t, "/api/projects/file?"+q+"&path=README.md"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"content":"hello"`) {
				t.Errorf("preview: %d %s", w.Code, w.Body.String())
			}
			if w := get(t, "/api/projects/file?project=demo&node="+nodeID+"&path=sub/a.txt&mode=raw"); w.Code != http.StatusOK || w.Body.String() != "in sub" ||
				w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("raw: %d %q %v", w.Code, w.Body.String(), w.Header())
			}
			if w := get(t, "/api/projects/file?"+q+"&path=big.bin&mode=download"); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), big) {
				t.Errorf("download: %d, %d bytes, want %d", w.Code, w.Body.Len(), len(big))
			}
			if w := get(t, "/api/projects/file?"+q+"&path=big.bin&mode=raw"); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("raw binary: %d, want 415 from this host's gate", w.Code)
			}
			if w := get(t, "/api/projects/file?"+q+"&path=.env&mode=download"); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "SECRET") {
				t.Errorf("sensitive download: %d %s", w.Code, w.Body.String())
			}
			if w := get(t, "/api/projects/file?"+q+"&path=escape"); w.Code != http.StatusNotFound {
				t.Errorf("symlink escape: %d", w.Code)
			}
			w := get(t, "/api/projects/files/list?"+q)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"big.bin"`) || !strings.Contains(w.Body.String(), `"root_name":"repo"`) {
				t.Errorf("list: %d %s", w.Code, w.Body.String())
			}

			body := `{"project":"demo","node":"` + nodeID + `","paths":["README.md","missing.txt"]}`
			ew := httptest.NewRecorder()
			h.HandleFilesExists(ew, httptest.NewRequest(http.MethodPost, "/api/projects/files/exists", strings.NewReader(body)))
			var res node.FilesExists
			_ = json.Unmarshal(ew.Body.Bytes(), &res)
			if ew.Code != http.StatusOK || !res.Results["README.md"].Exists || res.Results["README.md"].Size != 5 || res.Results["missing.txt"].Exists {
				t.Errorf("exists: %d %s", ew.Code, ew.Body.String())
			}
		})
	}

	t.Run("node without the files capability", func(t *testing.T) {
		for _, path := range []string{
			"/api/projects/file?" + root + "&node=old&path=README.md",
			"/api/projects/files/list?" + root + "&node=old",
		} {
			if w := get(t, path); w.Code != http.StatusNotImplemented || strings.Contains(w.Body.String(), "LOCAL") {
				t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
			}
		}
	})
	t.Run("unknown node", func(t *testing.T) {
		if w := get(t, "/api/projects/file?"+root+"&node=nope&path=README.md"); w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
	t.Run("local", func(t *testing.T) {
		if w := get(t, "/api/projects/file?"+root+"&node=local&path=README.md"); !strings.Contains(w.Body.String(), "LOCAL") {
			t.Errorf("local preview: %d %s", w.Code, w.Body.String())
		}
	})
}
