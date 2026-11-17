package memory

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
)

// viewFixture is a Claude directory with a projects root, and a handler on it.
type viewFixture struct {
	t        *testing.T
	claude   string
	projects string
	h        *Handler
	mux      *http.ServeMux
}

func newViewFixture(t *testing.T, plannerDir string, ws []Workspace) *viewFixture {
	t.Helper()
	claude, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(claude, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &viewFixture{t: t, claude: claude, projects: projects}
	f.h = New(Deps{ProjectsDir: projects, ClaudeMD: filepath.Join(claude, "CLAUDE.md"), PlannerDir: plannerDir,
		Workspaces: func() []Workspace { return ws }})
	f.mux = http.NewServeMux()
	for _, rt := range f.h.Routes() {
		f.mux.HandleFunc(rt.Pattern, rt.Handler)
	}
	return f
}

func (f *viewFixture) write(project, file, content string) string {
	f.t.Helper()
	dir := filepath.Join(f.projects, project, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// do runs a request; local makes it come from loopback straight to the
// listener.
func (f *viewFixture) do(method, target string, body any, local bool, hdr ...string) (*httptest.ResponseRecorder, map[string]any) {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, target, rd)
	if local {
		req.RemoteAddr = "127.0.0.1:52100"
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (f *viewFixture) list(local bool) listResponse {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/memories", nil)
	if local {
		req.RemoteAddr = "[::1]:52100"
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		f.t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	var resp listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *viewFixture) open(project, name string) fileResponse {
	f.t.Helper()
	w, _ := f.do(http.MethodGet, "/api/memories/file?project="+url.QueryEscape(project)+"&name="+url.QueryEscape(name), nil, false)
	if w.Code != http.StatusOK {
		f.t.Fatalf("open %s/%s = %d %s", project, name, w.Code, w.Body)
	}
	var resp fileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestListGroups: every project with memories is one group — the
// Planner's own first, then registered workspaces (matched by the encoded
// path whatever its case), then others named by a transcript's cwd or by
// the directory name, then temporary ones; empty projects, odd directory
// names, subdirectories and non-.md files are left out; entries carry the
// frontmatter, whether the index links them, and the group how many index
// lines link nothing.
func TestListGroups(t *testing.T) {
	planner := "/Users/me/.naozhi/planner-main"
	ws := []Workspace{{ID: "ws_aaaaaaaa", Name: "naozhi", Path: "/Users/me/Workspace/naozhi"}}
	f := newViewFixture(t, planner, ws)
	old := time.Now().Add(-48 * time.Hour)

	pl := claudefs.ProjectSlug(planner)
	f.write(pl, "style.md", "---\nname: style\ndescription: \"回复先结论 \\u003c3\"\nmetadata:\n  type: feedback\n  source: naozhi-planner\n  remembered_at: 2026-11-17T14:20:00+08:00\n---\n\n回复先结论。\n\n第二段\n")
	f.write(pl, "MEMORY.md", "- [style](style.md) — 回复先结论\n")

	nz := "-Users-me-workspace-naozhi" // lower-case w: the spelling sessions ran in
	touch(t, f.write(nz, "a-note.md", "---\nname: A note\ndescription: d\ntype: project\n---\nbody a\n"), old)
	f.write(nz, "plain.md", "no frontmatter here\n")
	f.write(nz, "MEMORY.md", "# 索引\n\n- [A](a-note.md) — d\n- [gone](gone.md) — x\n- [bak](a-note.md.bak) — y\n")
	f.write(nz, "notes.txt", "not a memory")
	if err := os.MkdirAll(filepath.Join(f.projects, nz, "memory", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	other := "-Users-me-Horizon-monitor"
	f.write(other, "x.md", "---\nname: x\n---\nx\n")
	if err := os.WriteFile(filepath.Join(f.projects, other, "s1.jsonl"), []byte(`{"type":"summary"}`+"\n"+`{"cwd":"/Users/me/Horizon-monitor","type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, f.write("-Users-me-nojsonl", "y.md", "y\n"), old)
	f.write("-private-tmp-iso1", "t.md", "t\n")
	// Empty memory directory, no memory directory, a name the CLI never
	// encodes: none is a group.
	if err := os.MkdirAll(filepath.Join(f.projects, "-Users-me-empty", "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.projects, "-Users-me-nomem"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("odd name", "z.md", "z\n")

	resp := f.list(false)
	if resp.Writable {
		t.Error("a request from 192.0.2.1 listed as writable")
	}
	var got []string
	for _, g := range resp.Groups {
		got = append(got, g.Kind+":"+g.Label)
	}
	want := []string{"planner:Planner 自己的", "workspace:naozhi", "other:/Users/me/Horizon-monitor", "other:-Users-me-nojsonl", "temp:-private-tmp-iso1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("groups = %v\nwant %v", got, want)
	}
	g0 := resp.Groups[0]
	if g0.WorkspaceID != "global" || len(g0.Entries) != 1 {
		t.Fatalf("planner group = %+v", g0)
	}
	e := g0.Entries[0]
	if e.Title != "style" || e.Type != "feedback" || e.Description != "回复先结论 <3" || e.Source != "naozhi-planner" ||
		e.RememberedAt != "2026-11-17T14:20:00+08:00" || e.Snippet != "回复先结论。" || !e.Indexed || e.ReadOnly != "" {
		t.Errorf("planner entry = %+v", e)
	}
	g1 := resp.Groups[1]
	if g1.WorkspaceID != "ws_aaaaaaaa" || g1.Project != nz || g1.Index == nil || g1.Index.Lines != 5 || g1.Dangling != 1 {
		t.Fatalf("workspace group = %+v (index %+v)", g1, g1.Index)
	}
	if len(g1.Entries) != 2 || g1.Entries[0].Name != "plain" || g1.Entries[0].Title != "plain" || g1.Entries[0].Indexed ||
		g1.Entries[1].Name != "a-note" || g1.Entries[1].Title != "A note" || g1.Entries[1].Type != "project" || !g1.Entries[1].Indexed {
		t.Errorf("workspace entries (newest first) = %+v", g1.Entries)
	}

	if !f.list(true).Writable {
		t.Error("a request from ::1 listed as read-only")
	}
}

// TestListTwoSpellings: on a case-sensitive file system two directories can
// both match one workspace; each group then names its directory too.
func TestListTwoSpellings(t *testing.T) {
	f := newViewFixture(t, "", []Workspace{{ID: "ws_bbbbbbbb", Name: "repo", Path: "/srv/Repo"}})
	f.write("-srv-Repo", "a.md", "a\n")
	if _, err := os.Stat(filepath.Join(f.projects, "-srv-repo")); err == nil {
		t.Skip("case-insensitive file system")
	}
	f.write("-srv-repo", "b.md", "b\n")
	resp := f.list(false)
	if len(resp.Groups) != 2 || resp.Groups[0].Label == resp.Groups[1].Label ||
		!strings.HasPrefix(resp.Groups[0].Label, "repo · -srv-") {
		t.Fatalf("groups = %+v", resp.Groups)
	}
}

// TestOpenFile: a file is read whole with its version; the index and the
// user's CLAUDE.md read too; a missing one is memory_gone; a bad parameter
// is refused.
func TestOpenFile(t *testing.T) {
	f := newViewFixture(t, "", nil)
	f.write("-p", "a.md", "---\nname: A\n---\n\nhello\n")
	f.write("-p", "MEMORY.md", "- [A](a.md)\n")
	got := f.open("-p", "a")
	if got.Text != "---\nname: A\n---\n\nhello\n" || got.Body != "hello\n" || got.Title != "A" || got.Version == "" || got.ReadOnly != "" {
		t.Errorf("open = %+v", got)
	}
	if idx := f.open("-p", "MEMORY"); idx.Body != "- [A](a.md)\n" || idx.Text != idx.Body {
		t.Errorf("index = %+v", idx)
	}
	if err := os.WriteFile(filepath.Join(f.claude, "CLAUDE.md"), []byte("# me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if md := f.open("", "CLAUDE"); md.Body != "# me\n" || md.Text != "" || md.ReadOnly != "claude_md" {
		t.Errorf("CLAUDE.md = %+v", md)
	}
	if f.list(false).ClaudeMD == nil {
		t.Error("CLAUDE.md not listed")
	}
	for target, want := range map[string]string{
		"/api/memories/file?project=-p&name=nope": "memory_gone",
		"/api/memories/file?project=-q&name=a":    "memory_gone",
		"/api/memories/file?project=..&name=a":    "invalid_project",
		"/api/memories/file?project=-p/..&name=a": "invalid_project",
		"/api/memories/file?project=-p&name=../a": "invalid_name",
		"/api/memories/file?project=-p&name=a.md": "invalid_name",
		"/api/memories/file?project=-p..x&name=a": "invalid_project",
		"/api/memories/file?project=&name=a":      "invalid_project",
	} {
		w, body := f.do(http.MethodGet, target, nil, false)
		if body["error"] != want {
			t.Errorf("%s = %d %v, want %s", target, w.Code, body, want)
		}
	}
}

// TestPut: an edit replaces the whole file when the version is the one
// opened; a stale version is 409 with the file as it is now; a missing file
// is 404 memory_gone and is not created; MEMORY.md is editable; size and
// UTF-8 are checked.
func TestPut(t *testing.T) {
	f := newViewFixture(t, "", nil)
	path := f.write("-p", "a.md", "v1\n")
	v1 := f.open("-p", "a").Version

	put := func(name, text, ver string) (*httptest.ResponseRecorder, map[string]any) {
		return f.do(http.MethodPut, "/api/memories/file", map[string]string{"project": "-p", "name": name, "text": text, "version": ver}, true)
	}
	w, body := put("a", "v2\n", v1)
	if w.Code != http.StatusOK || body["version"] == v1 {
		t.Fatalf("put = %d %v", w.Code, body)
	}
	if data, _ := os.ReadFile(path); string(data) != "v2\n" {
		t.Fatalf("file = %q", data)
	}
	// A CLI session wrote it after the page opened it.
	w, body = put("a", "mine\n", v1)
	if w.Code != http.StatusConflict || body["error"] != "memory_changed" || body["text"] != "v2\n" || body["version"] == "" {
		t.Fatalf("stale put = %d %v", w.Code, body)
	}
	if data, _ := os.ReadFile(path); string(data) != "v2\n" {
		t.Fatalf("stale put wrote: %q", data)
	}
	if w, body = put("new", "x", "1-1"); w.Code != http.StatusNotFound || body["error"] != "memory_gone" {
		t.Fatalf("put to a missing file = %d %v", w.Code, body)
	}
	if _, err := os.Stat(filepath.Join(f.projects, "-p", "memory", "new.md")); !os.IsNotExist(err) {
		t.Fatalf("put created a file: %v", err)
	}
	f.write("-p", "MEMORY.md", "- [a](a.md)\n")
	if w, body = put("MEMORY", "- [a](a.md) — 改\n", f.open("-p", "MEMORY").Version); w.Code != http.StatusOK {
		t.Fatalf("put MEMORY = %d %v", w.Code, body)
	}
	v2 := f.open("-p", "a").Version
	if w, body = put("a", strings.Repeat("x", maxMemoryFileBytes+1), v2); w.Code != http.StatusRequestEntityTooLarge || body["error"] != "memory_too_large" {
		t.Fatalf("too large = %d %v", w.Code, body)
	}
	if w, body = f.do(http.MethodPut, "/api/memories/file", map[string]string{"project": "-p", "name": "../a", "text": "x", "version": v2}, true); body["error"] != "invalid_name" {
		t.Fatalf("bad name = %d %v", w.Code, body)
	}
	if w, body = f.do(http.MethodPut, "/api/memories/file", map[string]string{"project": "../x", "name": "a", "text": "x", "version": v2}, true); body["error"] != "invalid_project" {
		t.Fatalf("bad project = %d %v", w.Code, body)
	}

	// A file over the cap is listed read-only and cannot be edited.
	big := f.write("-p", "big.md", strings.Repeat("y", maxMemoryFileBytes+10))
	st, _ := os.Stat(big)
	if w, _ = put("big", "small", version(st)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("editing a too-large file = %d", w.Code)
	}
	f.write("-p", "latin1.md", "caf\xe9\n")
	for _, e := range f.list(true).Groups[0].Entries {
		if want := map[string]string{"big": roTooLarge, "latin1": roNotUTF8}[e.Name]; e.ReadOnly != want {
			t.Errorf("%s readonly = %q, want %q", e.Name, e.ReadOnly, want)
		}
	}
}

// TestWritesOnlyFromLoopback (D126, decision B): PUT and DELETE are taken
// only from a request that reached the listener straight from loopback; a
// forwarding header never makes one local, whatever it says, and makes a
// loopback one (a proxy on this machine) remote.
func TestWritesOnlyFromLoopback(t *testing.T) {
	f := newViewFixture(t, "", nil)
	path := f.write("-p", "a.md", "v1\n")
	ver := f.open("-p", "a").Version
	body := map[string]string{"project": "-p", "name": "a", "text": "x", "version": ver}
	del := "/api/memories/file?project=-p&name=a&version=" + url.QueryEscape(ver)
	cases := []struct {
		name  string
		local bool
		hdr   []string
	}{
		{"remote address", false, nil},
		{"remote claiming loopback", false, []string{"X-Forwarded-For", "127.0.0.1"}},
		{"loopback proxy X-Forwarded-For", true, []string{"X-Forwarded-For", "203.0.113.9"}},
		{"loopback proxy X-Forwarded-Host", true, []string{"X-Forwarded-Host", "naozhi.example"}},
		{"loopback proxy Forwarded", true, []string{"Forwarded", "for=127.0.0.1"}},
		{"loopback proxy X-Real-IP", true, []string{"X-Real-IP", "127.0.0.1"}},
		{"loopback proxy Via", true, []string{"Via", "1.1 cloudfront"}},
		{"loopback CF-Connecting-IP", true, []string{"CF-Connecting-IP", "127.0.0.1"}},
	}
	for _, c := range cases {
		for _, m := range []string{http.MethodPut, http.MethodDelete} {
			target, b := "/api/memories/file", any(body)
			if m == http.MethodDelete {
				target, b = del, nil
			}
			w, out := f.do(m, target, b, c.local, c.hdr...)
			if w.Code != http.StatusForbidden || out["error"] != "memory_readonly_remote" {
				t.Errorf("%s %s = %d %v, want 403 memory_readonly_remote", c.name, m, w.Code, out)
			}
		}
		req := httptest.NewRequest(http.MethodGet, "/api/memories", nil)
		if c.local {
			req.RemoteAddr = "127.0.0.1:1"
		}
		for i := 0; i+1 < len(c.hdr); i += 2 {
			req.Header.Set(c.hdr[i], c.hdr[i+1])
		}
		if directLoopback(req) {
			t.Errorf("%s: listed as writable", c.name)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != "v1\n" {
		t.Fatalf("a refused write changed the file: %q", data)
	}
	// A reverse-node api call is an in-process request with no RemoteAddr.
	req, _ := http.NewRequest(http.MethodGet, "/api/memories", nil)
	if directLoopback(req) {
		t.Error("a request with no RemoteAddr is local")
	}
}

// TestSymlinksStayInside: a symlinked memory file or memory directory that
// resolves out of the projects root, or onto a file inside it that is not a
// memory file, is neither read nor written nor deleted.
func TestSymlinksStayInside(t *testing.T) {
	f := newViewFixture(t, "", nil)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.write("-p", "ok.md", "ok\n")
	mem := filepath.Join(f.projects, "-p", "memory")
	if err := os.Symlink(secret, filepath.Join(mem, "leak.md")); err != nil {
		t.Fatal(err)
	}
	// A transcript inside the root, reached as a memory file.
	tr := filepath.Join(f.projects, "-p", "s.jsonl.md")
	if err := os.WriteFile(tr, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tr, filepath.Join(mem, "tr.md")); err != nil {
		t.Fatal(err)
	}
	// A whole memory directory pointing out.
	if err := os.MkdirAll(filepath.Join(f.projects, "-q"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.projects, "-q", "memory")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(secret)
	ver := version(st)
	for _, c := range []struct{ project, name string }{{"-p", "leak"}, {"-p", "tr"}, {"-q", "secret"}} {
		q := "?project=" + c.project + "&name=" + c.name
		if w, out := f.do(http.MethodGet, "/api/memories/file"+q, nil, false); w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
			t.Errorf("read %s/%s = %d %v", c.project, c.name, w.Code, out)
		}
		if w, out := f.do(http.MethodPut, "/api/memories/file", map[string]string{"project": c.project, "name": c.name, "text": "pwned", "version": ver}, true); w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
			t.Errorf("write %s/%s = %d %v", c.project, c.name, w.Code, out)
		}
		if w, out := f.do(http.MethodDelete, "/api/memories/file"+q+"&version="+url.QueryEscape(ver), nil, true); w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
			t.Errorf("delete %s/%s = %d %v", c.project, c.name, w.Code, out)
		}
	}
	if data, _ := os.ReadFile(secret); string(data) != "secret\n" {
		t.Fatalf("outside file changed: %q", data)
	}
	if data, _ := os.ReadFile(tr); string(data) != "{}\n" {
		t.Fatalf("transcript changed: %q", data)
	}
	resp := f.list(false)
	if len(resp.Groups) != 1 || len(resp.Groups[0].Entries) != 1 || resp.Groups[0].Entries[0].Name != "ok" {
		t.Fatalf("list shows what the links point at: %+v", resp.Groups)
	}
}

// TestDelete: the file goes, then only the index lines linking exactly
// "(<name>.md)"; a stale version is 409 and deletes nothing; MEMORY.md
// cannot be deleted.
func TestDelete(t *testing.T) {
	f := newViewFixture(t, "", nil)
	path := f.write("-p", "name.md", "n\n")
	f.write("-p", "other-name.md", "o\n")
	index := "# 记忆\n\n- [n](name.md) — 这条\n- [bak](name.md.bak) — 备份\n- [o](other-name.md) — 别的\n- 又一次提到 (name.md)\n"
	idx := f.write("-p", "MEMORY.md", index)
	ver := f.open("-p", "name").Version

	if w, out := f.do(http.MethodDelete, "/api/memories/file?project=-p&name=name&version=1-2", nil, true); w.Code != http.StatusConflict || out["error"] != "memory_changed" {
		t.Fatalf("stale delete = %d %v", w.Code, out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale delete removed the file: %v", err)
	}
	w, out := f.do(http.MethodDelete, "/api/memories/file?project=-p&name=name&version="+url.QueryEscape(ver), nil, true)
	if w.Code != http.StatusOK || out["index_lines_removed"] != float64(2) {
		t.Fatalf("delete = %d %v", w.Code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file left: %v", err)
	}
	if data, _ := os.ReadFile(idx); string(data) != "# 记忆\n\n- [bak](name.md.bak) — 备份\n- [o](other-name.md) — 别的\n" {
		t.Fatalf("index = %q", data)
	}
	if w, out = f.do(http.MethodDelete, "/api/memories/file?project=-p&name=name&version="+url.QueryEscape(ver), nil, true); w.Code != http.StatusNotFound || out["error"] != "memory_gone" {
		t.Fatalf("second delete = %d %v", w.Code, out)
	}
	if w, out = f.do(http.MethodDelete, "/api/memories/file?project=-p&name=MEMORY&version=x", nil, true); w.Code != http.StatusBadRequest || out["error"] != "memory_index_undeletable" {
		t.Fatalf("delete MEMORY = %d %v", w.Code, out)
	}
}

// TestDeleteRacesRemember: the 懂你 delete and the remember block's index
// edit share claudefs.LockMemory, so run together under -race they leave an
// index that lists exactly the files that remain.
func TestDeleteRacesRemember(t *testing.T) {
	f := newViewFixture(t, "", nil)
	mem := filepath.Join(f.projects, "-p", "memory")
	const n = 20
	vers := make([]string, n)
	for i := range n {
		f.write("-p", fmt.Sprintf("old-%d.md", i), "o\n")
		vers[i] = f.open("-p", fmt.Sprintf("old-%d", i)).Version
	}
	var lines []string
	for i := range n {
		lines = append(lines, fmt.Sprintf("- [old-%d](old-%d.md)", i, i))
	}
	f.write("-p", "MEMORY.md", strings.Join(lines, "\n")+"\n")
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			w, _ := f.do(http.MethodDelete, fmt.Sprintf("/api/memories/file?project=-p&name=old-%d&version=%s", i, url.QueryEscape(vers[i])), nil, true)
			if w.Code != http.StatusOK {
				t.Errorf("delete old-%d = %d %s", i, w.Code, w.Body)
			}
		}()
		go func() {
			defer wg.Done()
			// What agent.Remember does under the lock.
			unlock := claudefs.LockMemory()
			defer unlock()
			name := fmt.Sprintf("new-%d", i)
			if err := os.WriteFile(filepath.Join(mem, name+".md"), []byte("n\n"), 0o644); err != nil {
				t.Error(err)
			}
			if _, err := claudefs.EditMemoryIndex(filepath.Join(mem, "MEMORY.md"), name, "- ["+name+"]("+name+".md)"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	g := f.list(false).Groups[0]
	if g.Dangling != 0 || len(g.Entries) != n || g.Index.Lines != n {
		t.Fatalf("after the race: %d entries, %d index lines, %d dangling", len(g.Entries), g.Index.Lines, g.Dangling)
	}
	for _, e := range g.Entries {
		if !strings.HasPrefix(e.Name, "new-") || !e.Indexed {
			t.Errorf("entry %+v", e)
		}
	}
}
