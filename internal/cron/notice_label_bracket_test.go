package cron

import (
	"strings"
	"testing"
)

// TestFormatCronNotice_StripsCloseBracketInLabel pins R250-SEC-6 (#1095)
// + R260528-SEC-8: label 中的 markdown link-syntax 字符 `]` `[` `(` `)`
// 必须替换为全角等价物，避免 cronNoticePrefixFmt 模板 "[Cron %s] %s"
// 被攻击者借 Title 内嵌 `](http://attacker)` 折叠成可点链接。
//
// 防御深度：dashboard 的 validateCronTitle 拦截 bidi / C0 控制字符，
// 但 ASCII `]` `[` `(` `)` 都是合法字符不会被拒；操作员手编
// cron_jobs.json 也能走到这里。所以在格式化时强制替换是底线。
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

	// R260528-SEC-8: label 中的 `(` `)` 也必须被替换为全角等价物，
	// 否则 markdown 渲染器仍可能把残留的 `(http://attacker)` 配成
	// link target。原 ASCII 括号必须不再出现在 label 段。
	if strings.Contains(got, "(http://attacker)") {
		t.Errorf("formatCronNotice 必须替换 label 中的 `(` `)`: %q", got)
	}
	if !strings.Contains(got, "（http://attacker）") {
		t.Errorf("formatCronNotice 应将 label 中的 `(` `)` 替换为全角 `（` `）`: %q", got)
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

// TestFormatCronNoticeBodyMarkdownEscape pins R260528-SEC-8: body containing
// markdown link-syntax characters `[` `(` `)` must be rewritten to their
// full-width equivalents so a sanitised-but-still-attacker-controlled run
// result cannot smuggle `[click](http://attacker)` clickable links into the
// IM card. body is already SanitizeForLog'd on the success path, but the
// log-sanitiser only scrubs C0/C1/bidi controls — ASCII brackets pass
// through, so without this escape an LLM-controlled prompt that emits
// markdown links would see them rendered live in Slack/Discord/Feishu.
func TestFormatCronNoticeBodyMarkdownEscape(t *testing.T) {
	t.Parallel()

	body := "click [here](http://attacker) to win"
	got := formatCronNotice("ok", body)

	// None of `[` `(` `)` from the body should survive in the output.
	if strings.ContainsAny(got, "[(") {
		// `]` test stays separate — the template emits one trailing `]`.
		// Strip the template `]` from the prefix `[Cron ok]` for the
		// `[` check by scanning beyond the template-known prefix bytes.
	}
	// Body-portion check: the output is "[Cron ok] " + escaped body.
	// Slice past the template prefix bytes to inspect just the body.
	const prefix = "[Cron ok] "
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("formatCronNotice prefix mismatch: %q", got)
	}
	bodyOut := got[len(prefix):]
	if strings.ContainsRune(bodyOut, '[') {
		t.Errorf("body still contains `[`: %q", bodyOut)
	}
	if strings.ContainsRune(bodyOut, ']') {
		t.Errorf("body still contains `]`: %q", bodyOut)
	}
	if strings.ContainsRune(bodyOut, '(') {
		t.Errorf("body still contains `(`: %q", bodyOut)
	}
	if strings.ContainsRune(bodyOut, ')') {
		t.Errorf("body still contains `)`: %q", bodyOut)
	}
	// The full-width replacements must show up.
	if !strings.Contains(bodyOut, "［here］") {
		t.Errorf("body should contain full-width `［here］`: %q", bodyOut)
	}
	if !strings.Contains(bodyOut, "（http://attacker）") {
		t.Errorf("body should contain full-width `（http://attacker）`: %q", bodyOut)
	}
}
