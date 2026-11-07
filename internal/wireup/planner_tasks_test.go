package wireup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/usermsg"
)

// A dispatched session runs on Claude whatever the general agent's backend,
// and a respawn rebuilds the dispatch spec from its sidecar.
func TestTaskOptsForKey(t *testing.T) {
	side := sidecar.Open("", nil)
	key := sidecar.TaskKey()
	if err := side.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Parent: sessionkey.PlannerMainKey,
		Title: "iso 写 hello", Dir: "/tmp/ws", Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	t.Run("respawn", func(t *testing.T) {
		to := TaskOpts{Base: session.AgentOpts{Model: "opus", Backend: "kiro", DefaultBackend: "kiro", SystemPrompt: "base"}, Sessions: side, Planner: "planner-abc123"}
		opts, ok := to.ForKey(key)
		if !ok || opts.Backend != "claude" || opts.DefaultBackend != "" || opts.Model != "sonnet" || opts.Workspace != "/tmp/ws" ||
			opts.Name != "iso 写 hello" || opts.CrossSessionInbound != "accept" || opts.Exempt ||
			!strings.HasPrefix(opts.SystemPrompt, "base\n\n") || !strings.Contains(opts.SystemPrompt, "SendMessage 发给 planner-abc123（") ||
			!strings.Contains(opts.SystemPrompt, "以 <naozhi-relay> 开头的消息由网关写入") || opts.MCPConfig != "" {
			t.Errorf("ForKey = %+v, %v", opts, ok)
		}
	})
	t.Run("no sidecar", func(t *testing.T) {
		if _, ok := (TaskOpts{Sessions: side}).ForKey(sidecar.TaskKey()); ok {
			t.Error("ForKey answered a key without a sidecar")
		}
	})
}

// A dispatched session's start that fails reaches the Planner as usermsg
// says it, a full max_procs with the limit.
func TestSpawnError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"max_procs full", fmt.Errorf("spawn: %w", session.ErrMaxProcs), usermsg.ForSendError(session.ErrMaxProcs, "") + "（max_procs 3）"},
		{"no CLI", session.ErrNoCLIWrapper, usermsg.ForSendError(session.ErrNoCLIWrapper, "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := spawnError(c.err, sidecar.TaskKey(), 3).Error(); got != c.want {
				t.Errorf("spawnError = %q, want %q", got, c.want)
			}
		})
	}
}

// The Planner is told a stopped task's promises the stop closed, so it does
// not close them again.
func TestStopReceipt(t *testing.T) {
	cases := []struct {
		name     string
		promises []string
		want     string // "" for no mention of promises
	}{
		{"none", nil, ""},
		{"two", []string{"pr_a", "pr_b"}, "相关的承诺 pr_a、pr_b 已一并标为已完成（任务已停止），不用再处理。"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stopReceipt("task:ab12", "iso 写 hello", c.promises)
			if !strings.HasPrefix(got, "用户在「任务」里停止了「iso 写 hello」（task:ab12）") || !strings.HasSuffix(got, "除非用户再要求。") {
				t.Errorf("receipt = %q", got)
			}
			// The Planner reads the receipt; "你" is the Planner there.
			if !strings.Contains(got, "已记为已完成（用户停止）") || strings.Contains(got, "你") {
				t.Errorf("receipt = %q, want it to say the user stopped it, without 你", got)
			}
			if c.want == "" && strings.Contains(got, "承诺") || c.want != "" && !strings.Contains(got, c.want) {
				t.Errorf("receipt = %q, want it to say %q", got, c.want)
			}
		})
	}
}

// A dispatch's mcp_env gives the session its own --mcp-config: every server
// of cli.mcp_config, the variables merged into those named; a server the
// config does not define fails the dispatch, and a respawn whose config no
// longer applies drops it rather than the session.
func TestTaskOptsMCPEnv(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(src, []byte(`{"mcpServers":{"aws-outlook-mcp":{"command":"aws-outlook-mcp","args":[],"env":{"A":"1"}},"exa":{"type":"http","url":"https://x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	side := sidecar.Open("", nil)
	to := TaskOpts{Sessions: side, Planner: "planner-abc123", MCPConfig: src,
		MCPPath: func(key string) string { return filepath.Join(dir, "out", key+".json") }}
	env := map[string]map[string]string{"aws-outlook-mcp": {"OUTLOOK_MCP_ENABLE_WRITES": "true"}}
	opts, err := to.For("task:ab12", "/tmp/ws", "建日程", "", env)
	if err != nil || opts.MCPConfig != filepath.Join(dir, "out", "task:ab12.json") {
		t.Fatalf("For = %+v, %v", opts.MCPConfig, err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	data, _ := os.ReadFile(opts.MCPConfig)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	outlook, _ := doc.MCPServers["aws-outlook-mcp"]["env"].(map[string]any)
	if outlook["A"] != "1" || outlook["OUTLOOK_MCP_ENABLE_WRITES"] != "true" || doc.MCPServers["exa"]["url"] != "https://x" {
		t.Errorf("written config = %s", data)
	}
	if fi, _ := os.Stat(opts.MCPConfig); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi)
	}

	if _, err := to.For("task:ab12", "/tmp/ws", "t", "", map[string]map[string]string{"slack": {"X": "1"}}); err == nil || !strings.Contains(err.Error(), "没有 MCP 服务器 slack") {
		t.Errorf("unknown server: %v", err)
	}
	if _, err := (TaskOpts{}).For("task:ab12", "/tmp/ws", "t", "", env); err == nil || !strings.Contains(err.Error(), "cli.mcp_config") {
		t.Errorf("no mcp_config: %v", err)
	}

	key := sidecar.TaskKey()
	if err := side.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Title: "t", Dir: "/tmp/ws", MCPEnv: map[string]map[string]string{"gone": {"X": "1"}}}); err != nil {
		t.Fatal(err)
	}
	if opts, ok := to.ForKey(key); !ok || opts.MCPConfig != "" || opts.Name != "t" {
		t.Errorf("respawn with a stale mcp_env = %+v, %v", opts, ok)
	}
}
