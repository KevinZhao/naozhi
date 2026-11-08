//go:build relaye2e

// Package relaye2e runs an isolated naozhi (its own port, state root, HOME,
// shim directory) with a fake Claude CLI and a fake gh, and checks the
// Planner's relay end to end (docs/rfc/planner.md D98): a relay in a turn the
// user started reaches the dispatched session with the user's words and the
// session acts on it; one in a receipt's turn is refused and never reaches
// it; an ask naming the task sends the user's answer straight there; a
// dispatch allowing one Outlook write tool starts its session with Outlook's
// write switch on and the other write tools denied.
//
//	go test -tags relaye2e -count=1 -v ./test/relaye2e/
package relaye2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const token = "relay-e2e-token"

type instance struct {
	t    *testing.T
	root string // everything of this run
	bin  string // naozhi, the fake claude, the fake gh, their logs
	base string
	cmd  *exec.Cmd
	out  *bytes.Buffer
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func start(t *testing.T) *instance {
	// Short: shim sockets live under it.
	root, err := os.MkdirTemp("/tmp", "nzr")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	in := &instance{t: t, root: root, bin: filepath.Join(root, "bin"), out: &bytes.Buffer{}}
	repo, _ := filepath.Abs("../..")
	run(t, repo, "go", "build", "-o", filepath.Join(in.bin, "naozhi"), "./cmd/naozhi/")
	run(t, repo, "go", "build", "-o", filepath.Join(in.bin, "claude"), "./test/relaye2e/fakeclaude/")
	gh := "#!/bin/sh\necho \"$@\" >> \"$(dirname \"$0\")/gh.log\"\necho merged\n"
	if err := os.WriteFile(filepath.Join(in.bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "ws")
	state := filepath.Join(root, "state")
	for _, d := range []string{ws, filepath.Join(state, "planner"), filepath.Join(root, "home"), filepath.Join(root, "run")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	reg := map[string]any{"v": 1, "workspaces": []map[string]any{{"id": "ws_e2etest1", "name": "e2e-ws",
		"paths": map[string][]string{"e2e": {ws}}, "created": time.Now().Format(time.RFC3339)}}}
	data, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(state, "planner", "workspaces.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	in.base = "http://127.0.0.1:" + strconv.Itoa(port)
	cfg := fmt.Sprintf(`server:
  addr: "127.0.0.1:%d"
  dashboard_token: %q
workspace:
  id: "e2e"
  name: "e2e"
cli:
  path: %q
  mcp_config: %q
session:
  cwd: %q
  store_path: %q
  shim:
    state_dir: %q
planner:
  enabled: true
`, port, token, filepath.Join(in.bin, "claude"), filepath.Join(root, "mcp.json"), ws, filepath.Join(state, "sessions.json"), filepath.Join(root, "shims"))
	// Never started: the fake claude runs no MCP server.
	mcp := `{"mcpServers":{"aws-outlook-mcp":{"command":"/nonexistent/aws-outlook-mcp","args":[],"env":{"A":"1"}}}}`
	if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte(mcp), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	in.cmd = exec.Command(filepath.Join(in.bin, "naozhi"), "--config", cfgPath)
	in.cmd.Dir = root
	in.cmd.Env = []string{"HOME=" + filepath.Join(root, "home"), "XDG_RUNTIME_DIR=" + filepath.Join(root, "run"), "PATH=" + in.bin + ":/usr/bin:/bin", "TMPDIR=" + root}
	in.cmd.Stdout, in.cmd.Stderr = in.out, in.out
	if err := in.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.stop)
	in.wait("naozhi up", 60*time.Second, func() bool {
		resp, err := http.Get(in.base + "/health")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	in.wait("Planner ready", 60*time.Second, func() bool {
		var st struct {
			Enabled bool `json:"enabled"`
			Alive   bool `json:"alive"`
		}
		in.get("/api/planner", &st)
		return st.Enabled && st.Alive
	})
	return in
}

// stop ends naozhi, then the shims and fake CLIs it started: only processes
// whose executable is in this run's bin directory.
func (in *instance) stop() {
	if in.cmd.Process != nil {
		_ = in.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = in.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = in.cmd.Process.Kill()
		}
	}
	out, _ := exec.Command("ps", "-axo", "pid=,command=").Output()
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasPrefix(f[1], in.bin+"/") {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if in.t.Failed() {
		in.t.Logf("naozhi output (tail):\n%s", tail(in.out.String(), 6000))
		for _, f := range []string{"received-planner.log", "received-task.log", "gh.log", "argv.log"} {
			b, _ := os.ReadFile(filepath.Join(in.bin, f))
			in.t.Logf("%s:\n%s", f, tail(string(b), 4000))
		}
		in.t.Logf("left in place: %s", in.root)
		return
	}
	_ = os.RemoveAll(in.root)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func (in *instance) req(method, path string, body any, out any) int {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, in.base+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (in *instance) get(path string, out any) int { return in.req("GET", path, nil, out) }

func (in *instance) send(text string) {
	in.t.Helper()
	if code := in.req("POST", "/api/planner/send", map[string]string{"text": text}, nil); code/100 != 2 {
		in.t.Fatalf("send %q: HTTP %d", text, code)
	}
}

func (in *instance) wait(what string, d time.Duration, ok func() bool) {
	in.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	in.t.Fatalf("timed out waiting for %s", what)
}

func (in *instance) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(in.bin, name))
	return string(b)
}

func (in *instance) notes() string {
	var resp struct {
		Notes []struct {
			Text string `json:"text"`
		} `json:"notes"`
	}
	in.get("/api/planner/notes", &resp)
	var b strings.Builder
	for _, n := range resp.Notes {
		b.WriteString(n.Text + "\n")
	}
	return b.String()
}

func TestRelayEndToEnd(t *testing.T) {
	in := start(t)

	// 1. The user asks for a dispatch; the dispatch's receipt starts a turn
	// the user did not start, in which the Planner tries to relay: refused.
	in.send("E2E-DISPATCH 派个会话看 PR")
	in.wait("the task dispatched", 60*time.Second, func() bool { return in.file("task_key") != "" })
	in.wait("the refusal told to the Planner", 60*time.Second, func() bool {
		return strings.Contains(in.file("received-planner.log"), "这一轮不是用户发起的")
	})
	if !strings.Contains(in.notes(), "不能转达用户的决定") {
		t.Errorf("no refusal note in the Planner conversation:\n%s", in.notes())
	}
	if got := in.file("received-task.log"); strings.Contains(got, "E2E-FROM-RECEIPT") || strings.Contains(got, "<naozhi-relay") {
		t.Fatalf("the receipt turn's relay reached the session:\n%s", got)
	}
	if gh := in.file("gh.log"); gh != "" {
		t.Fatalf("gh ran before any relay: %q", gh)
	}

	// 2. The user says it; the relay in that turn reaches the session in the
	// gateway's envelope, with the user's words, and the session acts.
	in.send("E2E-RELAY 用户拍板：合并 #1")
	in.wait("gh pr merge 1", 60*time.Second, func() bool { return strings.Contains(in.file("gh.log"), "pr merge 1") })
	got := in.file("received-task.log")
	for _, want := range []string{"<naozhi-relay at=", "在 Planner 对话里说：「E2E-RELAY 用户拍板：合并 #1」", "</naozhi-relay>\nPlanner 的说明：\nE2E-FROM-USER"} {
		if !strings.Contains(got, want) {
			t.Errorf("the session's message lacks %q:\n%s", want, got)
		}
	}

	// 3. An ask naming the task: the user's answer goes straight there.
	in.send("E2E-ASK")
	var id string
	in.wait("the question in the inbox", 30*time.Second, func() bool {
		var list struct {
			Open []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"open"`
		}
		in.get("/api/inbox", &list)
		for _, it := range list.Open {
			if strings.Contains(it.Title, "合并 #2") {
				id = it.ID
				return true
			}
		}
		return false
	})
	if code := in.req("POST", "/api/inbox/decide", map[string]string{"id": id, "action": "合并 #2"}, nil); code/100 != 2 {
		t.Fatalf("decide: HTTP %d", code)
	}
	in.wait("gh pr merge 2", 60*time.Second, func() bool { return strings.Contains(in.file("gh.log"), "pr merge 2") })
	if !strings.Contains(in.file("received-task.log"), "Planner 在通知里问用户「合并 #2 吗？」，用户") {
		t.Errorf("the answer's envelope:\n%s", in.file("received-task.log"))
	}
	in.wait("the Planner told the answer got there", 30*time.Second, func() bool {
		return strings.Contains(in.file("received-planner.log"), "网关已把这个回答原样送到 ")
	})
	if gh := in.file("gh.log"); strings.Count(gh, "pr merge") != 2 || strings.Contains(gh, "merge 9") {
		t.Errorf("gh calls = %q, want merges of #1 and #2 only", gh)
	}

	// 4. The user asks for a meeting: the dispatch allows calendar_meeting
	// alone; the session starts with Outlook's switch on in its own
	// --mcp-config and every other Outlook write tool denied in its settings.
	in.send("E2E-TOOLS 帮我建个日程")
	var argv []string
	in.wait("the e2e-cal session started", 60*time.Second, func() bool {
		for _, line := range strings.Split(in.file("argv.log"), "\n") {
			if name, args, ok := strings.Cut(line, "\t"); ok && name == "e2e-cal" {
				argv = unquoteArgs(args)
				return true
			}
		}
		return false
	})
	flag := func(f string) string {
		for i, a := range argv {
			if a == f && i+1 < len(argv) {
				return argv[i+1]
			}
		}
		t.Fatalf("argv lacks %s: %q", f, argv)
		return ""
	}
	var mcpDoc struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	b, _ := os.ReadFile(flag("--mcp-config"))
	if err := json.Unmarshal(b, &mcpDoc); err != nil || mcpDoc.MCPServers["aws-outlook-mcp"].Env["OUTLOOK_MCP_ENABLE_WRITES"] != "true" {
		t.Errorf("the session's --mcp-config: %s (%v)", b, err)
	}
	var settings struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	b, _ = os.ReadFile(flag("--settings"))
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatalf("the session's --settings: %s (%v)", b, err)
	}
	deny := strings.Join(settings.Permissions.Deny, " ")
	for _, want := range []string{"mcp__aws-outlook-mcp__email_send", "mcp__aws-outlook-mcp__enable_write_tools"} {
		if !strings.Contains(deny, want+" ") && !strings.HasSuffix(deny, want) {
			t.Errorf("deny lacks %s: %v", want, settings.Permissions.Deny)
		}
	}
	if strings.Contains(deny, "calendar_meeting") {
		t.Errorf("the allowed tool is denied: %v", settings.Permissions.Deny)
	}
}

// unquoteArgs reads back the fake claude's argv as logged (%q of a []string).
func unquoteArgs(s string) []string {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	var out []string
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		q, err := strconv.QuotedPrefix(s)
		if err != nil {
			return out
		}
		v, _ := strconv.Unquote(q)
		out = append(out, v)
		s = s[len(q):]
	}
	return out
}
