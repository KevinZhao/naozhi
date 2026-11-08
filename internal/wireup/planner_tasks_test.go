package wireup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/planner/action"
	"github.com/naozhi/naozhi/internal/planner/digest"
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
			opts.Name != "iso 写 hello" || opts.CrossSessionInbound != "accept" || opts.Exempt || opts.PromptCacheTTL != "" ||
			!strings.HasPrefix(opts.SystemPrompt, "base\n\n") || !strings.Contains(opts.SystemPrompt, "SendMessage 给 planner-abc123（") ||
			!strings.Contains(opts.SystemPrompt, "在这一轮回复的末尾写汇报块") || !strings.Contains(opts.SystemPrompt, "```naozhi-report\n{\"state\"") ||
			!strings.Contains(opts.SystemPrompt, "不要再用 SendMessage 汇报") ||
			!strings.Contains(opts.SystemPrompt, "以 <naozhi-relay> 开头的消息由网关写入") ||
			!strings.Contains(opts.SystemPrompt, `交代以外的改动不做，包括 PR 标题、描述和分支名；你觉得该改的写进汇报的"建议"`) || opts.MCPConfig != "" {
			t.Errorf("ForKey = %+v, %v", opts, ok)
		}
	})
	t.Run("ultracode", func(t *testing.T) {
		uc := sidecar.TaskKey()
		if err := side.Put(sidecar.Meta{Key: uc, Purpose: sidecar.PurposeTask, Title: "多轮编排", Dir: "/tmp/ws", Ultracode: true}); err != nil {
			t.Fatal(err)
		}
		to := TaskOpts{Sessions: side, Planner: "planner-abc123"}
		if opts, ok := to.ForKey(uc); !ok || !opts.Ultracode {
			t.Errorf("ForKey(ultracode) = %+v, %v", opts, ok)
		}
		if opts, _ := to.ForKey(key); opts.Ultracode {
			t.Error("a dispatch without ultracode respawned in it")
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

// A dispatch's mcp_tools give the session its own --mcp-config, every
// server of cli.mcp_config with the write switch of those named turned on,
// and a deny rule for each of their write tools not allowed (the one that
// turns them all on included, once); a server the config does not define fails
// the dispatch, and a respawn whose config no longer applies drops it
// rather than the session.
func TestTaskOptsMCPTools(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(src, []byte(`{"mcpServers":{"aws-outlook-mcp":{"command":"aws-outlook-mcp","args":[],"env":{"A":"1"}},"exa":{"type":"http","url":"https://x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	side := sidecar.Open("", nil)
	to := TaskOpts{Sessions: side, Planner: "planner-abc123", MCPConfig: src,
		Base:    session.AgentOpts{PermissionsDeny: []string{"Bash(rm -rf /)"}},
		MCPPath: func(key string) string { return filepath.Join(dir, "out", key+".json") }}
	tools := map[string][]string{"aws-outlook-mcp": {"calendar_meeting"}}
	opts, err := to.For("task:ab12", action.Spawn{Dir: "/tmp/ws", Title: "建日程", MCPTools: tools})
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
	for _, want := range []string{"Bash(rm -rf /)", "mcp__aws-outlook-mcp__email_send", "mcp__aws-outlook-mcp__enable_write_tools", "mcp__aws-outlook-mcp__calendar_room_booking"} {
		if !slices.Contains(opts.PermissionsDeny, want) {
			t.Errorf("deny %v lacks %s", opts.PermissionsDeny, want)
		}
	}
	if n := countOf(opts.PermissionsDeny, enableWrites); n != 1 {
		t.Errorf("deny %v holds enable_write_tools %d times", opts.PermissionsDeny, n)
	}
	if slices.Contains(opts.PermissionsDeny, "mcp__aws-outlook-mcp__calendar_meeting") || len(to.Base.PermissionsDeny) != 1 {
		t.Errorf("deny %v (base %v): the allowed tool denied or the base changed", opts.PermissionsDeny, to.Base.PermissionsDeny)
	}
	if plain, _ := to.For("task:cd34", action.Spawn{Dir: "/tmp/ws", Title: "t"}); plain.MCPConfig != "" || !slices.Equal(plain.PermissionsDeny, append([]string{"Bash(rm -rf /)"}, taskDenyRules...)) {
		t.Errorf("no mcp_tools = %+v", plain)
	}

	noOutlook := filepath.Join(dir, "exa-only.json")
	if err := os.WriteFile(noOutlook, []byte(`{"mcpServers":{"exa":{"type":"http","url":"https://x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	gone := to
	gone.MCPConfig = noOutlook
	if _, err := gone.For("task:ab12", action.Spawn{Dir: "/tmp/ws", Title: "t", MCPTools: tools}); err == nil || !strings.Contains(err.Error(), "没有 MCP 服务器 aws-outlook-mcp") {
		t.Errorf("server not in config: %v", err)
	}
	if _, err := to.For("task:ab12", action.Spawn{Dir: "/tmp/ws", Title: "t", MCPTools: map[string][]string{"slack-mcp": {"post_message"}}}); err == nil || !strings.Contains(err.Error(), "没有写开关") {
		t.Errorf("server with no switch: %v", err)
	}
	if _, err := (TaskOpts{}).For("task:ab12", action.Spawn{Dir: "/tmp/ws", Title: "t", MCPTools: tools}); err == nil || !strings.Contains(err.Error(), "cli.mcp_config") {
		t.Errorf("no mcp_config: %v", err)
	}

	key := sidecar.TaskKey()
	if err := side.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Title: "t", Dir: "/tmp/ws", MCPTools: tools}); err != nil {
		t.Fatal(err)
	}
	if opts, ok := gone.ForKey(key); !ok || opts.MCPConfig != "" || opts.Name != "t" || !slices.Equal(opts.PermissionsDeny, append([]string{"Bash(rm -rf /)"}, taskDenyRules...)) {
		t.Errorf("respawn with a stale mcp_tools = %+v, %v", opts, ok)
	}
	if opts, ok := to.ForKey(key); !ok || opts.MCPConfig == "" || !slices.Contains(opts.PermissionsDeny, "mcp__aws-outlook-mcp__email_send") {
		t.Errorf("respawn = %+v, %v", opts, ok)
	}
}

// enableWrites is the rule every dispatched session denies (taskDeny).
const enableWrites = "mcp__aws-outlook-mcp__enable_write_tools"

// taskDenyRules is taskDeny written out: the tool, and the two Bash rules
// against starting Outlook's server with its write switch.
var taskDenyRules = []string{enableWrites, "Bash(aws-outlook-mcp:*)", "Bash(*OUTLOOK_MCP_ENABLE_WRITES*)"}

func countOf(list []string, v string) (n int) {
	for _, s := range list {
		if s == v {
			n++
		}
	}
	return n
}

// TestTaskOptsDenyEnableWrites: a dispatched session with no mcp_tools and
// no cli.mcp_config still denies Outlook's enable_write_tools, on the first
// spawn and on a respawn from its sidecar, and the base options (shared by
// every dispatch) are left as they were.
func TestTaskOptsDenyEnableWrites(t *testing.T) {
	side := sidecar.Open("", nil)
	to := TaskOpts{Sessions: side, Planner: "planner-abc123"}
	opts, err := to.For("task:ab12", action.Spawn{Dir: "/tmp/ws", Title: "只读"})
	if err != nil || !slices.Equal(opts.PermissionsDeny, taskDenyRules) {
		t.Fatalf("For = %v, %v", opts.PermissionsDeny, err)
	}
	if to.Base.PermissionsDeny != nil {
		t.Errorf("base changed: %v", to.Base.PermissionsDeny)
	}
	key := sidecar.TaskKey()
	if err := side.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Title: "只读", Dir: "/tmp/ws"}); err != nil {
		t.Fatal(err)
	}
	if opts, ok := to.ForKey(key); !ok || !slices.Equal(opts.PermissionsDeny, taskDenyRules) {
		t.Errorf("respawn = %v, %v", opts.PermissionsDeny, ok)
	}
}

// The task prompt's report block is one ParseReport takes once its
// placeholders are filled in; the note after the first message asks for it
// too and holds no full-width closing parenthesis before its end, which the
// dashboard strips the note up to.
func TestTaskPromptReport(t *testing.T) {
	p := taskPrompt("planner-abc123")
	i := strings.Index(p, "```naozhi-report")
	if i < 0 {
		t.Fatalf("prompt has no report block: %s", p)
	}
	j := strings.Index(p[i+3:], "```")
	reply := "正文\n" + strings.NewReplacer("done | blocked | need_user | progress", "done", "不超过 120 字，第一句写结论", "写好了").Replace(p[i:i+3+j+3])
	if rep, body, ok, err := action.ParseReport(reply); !ok || err != nil || rep.State != "done" || body != "正文" {
		t.Errorf("ParseReport(%q) = %+v, %q, %v, %v", reply, rep, body, ok, err)
	}
	note := reportLine("planner-abc123")
	inner := strings.TrimSuffix(strings.TrimSpace(note), "）")
	if !strings.HasPrefix(strings.TrimSpace(note), "（网关附注：") || strings.Contains(inner, "）") ||
		!strings.Contains(note, "naozhi-report 汇报块") || !strings.Contains(note, "SendMessage 给 planner-abc123") {
		t.Errorf("reportLine = %q", note)
	}
}

// A dispatch's digest keeps its goal, else the first paragraph of the
// Planner's own text of the message (brief default 3), not of the user's
// words the envelope leads it with.
func TestDispatchGoal(t *testing.T) {
	cases := []struct {
		s    action.Spawn
		want string
	}{
		{action.Spawn{Goal: "让 CI 变绿", Message: "看一下 CI。然后修。"}, "让 CI 变绿"},
		{action.Spawn{Message: "看一下 CI 为什么红。修好后开 PR。\n\n范围：只动测试。"}, "看一下 CI 为什么红。修好后开 PR。"},
		{action.Spawn{Message: "<naozhi-relay at=\"11/8 10:00\">\n用户 11/8 10:00 在 Planner 对话里说：「合并吧。」\n</naozhi-relay>\nPlanner 的说明：\n合并 #1。再发版"}, "合并 #1。再发版"},
	}
	for _, c := range cases {
		if got := digest.Goal(dispatchGoal(c.s)); got != c.want {
			t.Errorf("digest.Goal(dispatchGoal(%+v)) = %q, want %q", c.s, got, c.want)
		}
	}
}

// The snapshot's conclusion is the first sentence of the sidecar's report.
func TestPlannerConclusion(t *testing.T) {
	side := sidecar.Open("", nil)
	key := sidecar.TaskKey()
	if err := side.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask}); err != nil {
		t.Fatal(err)
	}
	c := PlannerConclusion(side)
	if got := c(key); got != "" {
		t.Errorf("conclusion with no report = %q", got)
	}
	side.Update(key, func(m *sidecar.Meta) bool {
		m.Report = &sidecar.Report{State: "done", Summary: "测试全过。文档没动"}
		return true
	})
	if got := c(key); got != "测试全过。" {
		t.Errorf("conclusion = %q", got)
	}
	if got := c("task:nope"); got != "" {
		t.Errorf("conclusion of no sidecar = %q", got)
	}
}
