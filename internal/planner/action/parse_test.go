package action

import (
	"strings"
	"testing"
	"time"
)

func block(body string) string { return "```naozhi-action\n" + body + "\n```" }

func TestParse(t *testing.T) {
	due := time.Date(2026, 11, 5, 9, 0, 0, 0, time.FixedZone("", 8*3600))
	cases := []struct {
		name string
		text string
		// want is each block's type, or "!" + a substring of its error.
		want []string
	}{
		{"none", "只是回答，没有动作块。\n```go\nfmt.Println()\n```", nil},
		{"dispatch", "好的。\n" + block(`{"type":"dispatch","workspace":"naozhi","title":"写 hello","model":"","message":"写一个 hello.md"}`), []string{"dispatch"}},
		{"promise", block(`{"type":"promise","text":"明早提醒你看 CI","due":"2026-11-05T09:00:00+08:00","close_when":"你看过"}`), []string{"promise"}},
		{"promise without due", block(`{"type":"promise","text":"盯着发版","due":"","close_when":""}`), []string{"promise"}},
		{"ask", block(`{"type":"ask","question":"今天发版吗？","options":["发","不发"]}`), []string{"ask"}},
		{"indented fence", "1. 派活：\n   ```naozhi-action\n   {\"type\":\"ask\",\"question\":\"好吗\",\"options\":[]}\n   ```", []string{"ask"}},
		{"several", block(`{"type":"promise","text":"a","due":"","close_when":""}`) + "\n中间的话\n" + block(`{"type":"ask","question":"b","options":["x"]}`), []string{"promise", "ask"}},
		{"unknown type", block(`{"type":"halt","key":"task:1"}`), []string{"!未知类型"}},
		{"missing type", block(`{"text":"a"}`), []string{"!缺少 type"}},
		{"not json", block(`type: dispatch`), []string{"!不是一个 JSON 对象"}},
		{"unknown field", block(`{"type":"promise","text":"a","due":"","close_when":"","extra":1}`), []string{"!字段不对"}},
		{"two objects", block(`{"type":"ask","question":"a","options":[]} {"type":"ask","question":"b","options":[]}`), []string{"!不是一个 JSON 对象"}},
		{"bad due", block(`{"type":"promise","text":"a","due":"明天","close_when":""}`), []string{"!RFC3339"}},
		{"dispatch no message", block(`{"type":"dispatch","workspace":"w","title":"t","model":"","message":" "}`), []string{"!缺少 message"}},
		{"dispatch bad model", block(`{"type":"dispatch","workspace":"w","title":"t","model":"--dangerous","message":"m"}`), []string{"!model"}},
		{"dispatch title newline", block(`{"type":"dispatch","workspace":"w","title":"a\nb","model":"","message":"m"}`), []string{"!控制字符"}},
		{"ask duplicate options", block(`{"type":"ask","question":"q","options":["是"," 是"]}`), []string{"!重复"}},
		{"unterminated", "```naozhi-action\n{\"type\":\"ask\",\"question\":\"q\",\"options\":[]}", []string{"!没有结束"}},
		{"bad block does not stop the next", block(`{"type":"nope"}`) + "\n" + block(`{"type":"ask","question":"q","options":[]}`), []string{"!未知类型", "ask"}},
		{"status", block(`{"type":"status","task":"task:ab12","state":"done","why":"它汇报 PR 已合并"}`), []string{"status"}},
		{"status of a promise", block(`{"type":"status","task":"pr_x","state":"doing","why":"条件还没满足"}`), []string{"status"}},
		{"status bad state", block(`{"type":"status","task":"task:ab12","state":"closed","why":"w"}`), []string{"!state 只能是"}},
		{"status no task", block(`{"type":"status","task":" ","state":"done","why":"w"}`), []string{"!缺少 task"}},
		{"status no why", block(`{"type":"status","task":"task:ab12","state":"failed","why":""}`), []string{"!缺少 why"}},
		{"status unknown field", block(`{"type":"status","task":"task:ab12","state":"done","why":"w","by":"user"}`), []string{"!字段不对"}},
		{"too many", strings.Repeat(block(`{"type":"ask","question":"q","options":[]}`)+"\n", 6), []string{"ask", "ask", "ask", "ask", "ask", "!最多 5 个"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d blocks %+v, want %d", len(got), got, len(tc.want))
			}
			for i, b := range got {
				w := tc.want[i]
				if msg, bad := strings.CutPrefix(w, "!"); bad {
					if b.Err == nil || !strings.Contains(b.Err.Error(), msg) {
						t.Errorf("block %d: err = %v, want one containing %q", i, b.Err, msg)
					}
					if b.Dispatch != nil || b.Promise != nil || b.Ask != nil || b.Status != nil {
						t.Errorf("block %d: refused block still carries an action", i)
					}
					continue
				}
				if b.Err != nil || string(b.Type) != w {
					t.Errorf("block %d: type %q err %v, want %q", i, b.Type, b.Err, w)
				}
			}
		})
	}

	b := Parse(block(`{"type":"promise","text":" 明早提醒你看 CI ","due":"2026-11-05T09:00:00+08:00","close_when":"你看过"}`))[0]
	if b.Promise == nil || b.Promise.Text != "明早提醒你看 CI" || !b.Promise.Due.Equal(due) || b.Promise.CloseWhen != "你看过" {
		t.Errorf("promise = %+v", b.Promise)
	}
	d := Parse(block(`{"type":"dispatch","workspace":"ws_1","title":"写 hello","model":"claude-sonnet-5-5","message":"写一个 hello.md"}`))[0].Dispatch
	if d == nil || *d != (Dispatch{Workspace: "ws_1", Title: "写 hello", Model: "claude-sonnet-5-5", Message: "写一个 hello.md"}) {
		t.Errorf("dispatch = %+v", d)
	}
}
