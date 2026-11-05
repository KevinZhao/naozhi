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
		"<会话 key>", "dashboard:direct:u:general",
	)
	formats := regexp.MustCompile(`\{"type":"[a-z]+"[^\n]*?\}`).FindAllString(p, -1)
	if len(formats) != 7 {
		t.Fatalf("found %d block formats, want 7: %q", len(formats), formats)
	}
	for _, f := range formats {
		body := strings.ReplaceAll(fill.Replace(f), `["x"]`, `["是"]`)
		if b := action.Parse("```naozhi-action\n" + body + "\n```"); len(b) != 1 || b[0].Err != nil {
			t.Errorf("format %s does not parse: %+v", body, b)
		}
	}
}
