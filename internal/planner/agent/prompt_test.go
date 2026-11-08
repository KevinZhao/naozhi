package agent

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/planner/action"
)

// The system prompt renders whole, and every block format it spells out is
// one the gateway parses once its placeholders are filled in.
func TestSystemPrompt(t *testing.T) {
	p := systemPrompt("/state/planner", "planner-abc123", []string{"~/.claude/projects/x/memory/MEMORY.md"})
	for _, want := range []string{
		fmt.Sprintf("提示词版本 %d", PromptVersion),
		"/state/planner/index.json",
		"  - ~/.claude/projects/x/memory/MEMORY.md\n",
		"<naozhi-scout>",
		"超过 4 小时没回",
		"巡检触发的回合只能用 notify、promise、ask",
		"你的跨会话名字是 planner-abc123",
		`不要写成"提醒用户提交周报"`,
		"不要改用 SendMessage 绕开",
		"也不要另派会话接着做同一件事",
		"一律用 ask 问用户，不替用户答",
		`"with_user_words":true`,
		"不要叫会话去改 ~/.claude.json",
		"用户的决定只用 relay 或带 task 的 ask 送给会话",
		`<naozhi-snapshot unchanged since="HH:MM"/>`,
		"健康状态、最后输出（last_active）",
		"读它一律用 Read 的 offset / limit 分段读，不要整份读",
		"搜记忆只在 ~/.claude/projects/*/memory 下搜，不要从 ~/.claude/projects 根目录搜",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "%!") {
		t.Error("prompt has a bad format verb")
	}
	// The subscription a dispatch once asked for died with the conversation
	// at a shift change; the gateway's receipt replaces it.
	if strings.Contains(p, "notify_when_idle") {
		t.Error("prompt still asks for notify_when_idle")
	}
	fill := strings.NewReplacer(
		"<工作区 ID 或名字>", "naozhi", "<RFC3339 或空>", "", "<task: key 或承诺 ID>", "task:ab12",
		"done | doing | failed", "done", "timely | brief", "timely", "<http(s) 链接或空>", "", "<巡检发现的 id 或空>", "", "…", "x",
		"<会话 key>", "dashboard:direct:u:general", "<会话 key 或空>", "",
	)
	formats := regexp.MustCompile(`\{"type":"[a-z]+"[^\n]*?\}`).FindAllString(p, -1)
	if len(formats) != 8 {
		t.Fatalf("found %d block formats, want 8: %q", len(formats), formats)
	}
	for _, f := range formats {
		body := strings.ReplaceAll(fill.Replace(f), `["x"]`, `["是"]`)
		if b := action.Parse("```naozhi-action\n" + body + "\n```"); len(b) != 1 || b[0].Err != nil {
			t.Errorf("format %s does not parse: %+v", body, b)
		}
	}
}
