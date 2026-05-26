package cron

import (
	"strings"
	"testing"
)

// TestFormatCronNotice_StripsCloseBracketInLabel pins R250-SEC-6 (#1095):
// label 中的 `]` 必须替换为全角 `］`，避免 cronNoticePrefixFmt 模板
// "[Cron %s] %s" 被攻击者借 Title 内嵌 `]` 提前闭合。无此处理时一个
// 形如 "evil](http://attacker)" 的 Title 在 markdown-aware IM channel
// 渲染下会折叠成可点链接：`[Cron evil](http://attacker) ...`。
//
// 防御深度：dashboard 的 validateCronTitle 拦截 bidi / C0 控制字符，
// 但 ASCII `]` 是合法字符不会被拒；操作员手编 cron_jobs.json 也能
// 走到这里。所以在格式化时强制替换是底线。
func TestFormatCronNotice_StripsCloseBracketInLabel(t *testing.T) {
	t.Parallel()

	label := "evil](http://attacker)"
	got := formatCronNotice(label, "body")

	// 替换后 label 段应该只剩一个 `]`：模板末尾的那个。任何额外的 `]`
	// 都意味着 label 内的 `]` 没被替换。
	bracketCount := strings.Count(got, "]")
	if bracketCount != 1 {
		t.Errorf("formatCronNotice 输出含 %d 个 `]`，应为 1（仅模板末尾的那个）: %q",
			bracketCount, got)
	}

	// 全角替代字符必须存在，证明替换确实发生了。
	if !strings.Contains(got, "］") {
		t.Errorf("formatCronNotice 未把 label 中的 `]` 替换为全角 `］`: %q", got)
	}

	// 后缀 body 不能受影响。
	if !strings.HasSuffix(got, "] body") {
		t.Errorf("formatCronNotice 末尾应保留 `] body`，得到 %q", got)
	}

	// 原始 label 中的 `(` 等其它内容应该保留——本修复只针对 `]`。
	if !strings.Contains(got, "(http://attacker)") {
		t.Errorf("formatCronNotice 不应改写 `]` 之外的 label 字符: %q", got)
	}
}

// TestFormatCronNotice_NoBracketInLabelUnchanged 普通 label 不含 `]`
// 时输出与原 label 一字不差，确保替换是 idempotent + 零副作用。
func TestFormatCronNotice_NoBracketInLabelUnchanged(t *testing.T) {
	t.Parallel()

	label := "daily-review"
	got := formatCronNotice(label, "body")
	want := "[Cron daily-review] body"
	if got != want {
		t.Errorf("formatCronNotice(%q, body) = %q, want %q", label, got, want)
	}
}
