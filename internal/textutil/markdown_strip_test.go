package textutil

import "testing"

// TestStripMarkdown pins the sidebar-preview cleanup for #2435 item 1: the
// session card's second line rendered raw "## 判分", "**≈129.5/150**" and
// "`b466411`" because the assistant summary was truncated but never stripped.
func TestStripMarkdown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain", "hello world", "hello world"},
		{"plain cjk", "判分完成，总分 129.5", "判分完成，总分 129.5"},
		{"h2 cjk", "## 判分", "判分"},
		{"h1 with trailing text lines", "# 标题\n\n正文 **≈129.5/150**", "标题 正文 ≈129.5/150"},
		{"bold cjk", "**≈129.5/150**", "≈129.5/150"},
		{"inline code", "提交 `b466411` 已合并", "提交 b466411 已合并"},
		{"link", "见 [判分说明](https://example.com/a?b=1) 部分", "见 判分说明 部分"},
		{"truncated link", "见 [判分说明](https://example.com/very/long...", "见 判分说明"},
		{"image", "![截图](https://x/y.png) 完成", "截图 完成"},
		{"nested emphasis code link", "**bold with `code` and [link](http://x)**", "bold with code and link"},
		{"nested quote heading list", "> ## 引用标题\n> - 列表项 *斜体*\n> 1. 有序", "引用标题 列表项 斜体 有序"},
		{"list bullets", "- 第一\n* 第二\n+ 第三\n2) 第四", "第一 第二 第三 第四"},
		{"hashtag not heading", "#123 与 #tag 保留", "#123 与 #tag 保留"},
		{"bare hashes fall back to input", "###", "###"},
		{"only fence falls back to input", "```", "```"},
		{"math asterisk kept", "5*3=15", "5*3=15"},
		{"glob asterisk kept", "a/*.go 与 b/**/c", "a/*.go 与 b/**/c"},
		{"dunder file kept", "__init__.py 已创建", "__init__.py 已创建"},
		{"dunder emphasis end", "__加粗__", "加粗"},
		{"underscore emphasis before cjk punct", "_强调_，然后", "强调，然后"},
		{"asterisk emphasis in parens", "(*x*) 与 *y*.", "(x) 与 y."},
		{"unclosed bold literal", "**判分结果...", "**判分结果..."},
		{"space padded asterisks literal", "a * b * c", "a * b * c"},
		{"four tildes not a fence (list marker inside is stripped)", "~~~~\n- raw\n~~~~", "raw"},
		{"tilde fence with info", "~~~go\n**x**\n~~~", "**x**"},
		{"rule dropped", "上\n---\n下\n***", "上 下"},
		{"closed fence keeps code raw", "结果：\n```go\nfmt.Println(\"**x**\")\n```\n完成", "结果： fmt.Println(\"**x**\") 完成"},
		{"unclosed fence", "```bash\ngit log --oneline\nb466411 fix", "git log --oneline b466411 fix"},
		{"tilde fence", "~~~\n- raw\n~~~", "- raw"},
		{"strikethrough", "~~旧~~ 新", "旧 新"},
		{"underscore emphasis", "_强调_ 与 __加粗__", "强调 与 加粗"},
		{"snake_case kept", "调用 my_func_name 完成", "调用 my_func_name 完成"},
		{"literal asterisk kept", "5 * 3 = 15", "5 * 3 = 15"},
		{"whitespace collapse", "  a  \n\n\n  b\t c ", "a b c"},
		{"ellipsis survives", "## 判分结果很长很长...", "判分结果很长很长..."},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := StripMarkdown(tc.in); got != tc.want {
				t.Errorf("StripMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStripMarkdown_NoNewline guards the single-line contract every caller
// relies on: whatever the input, the output never carries a line break.
func TestStripMarkdown_NoNewline(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"a\nb", "a\n\n\nb", "```\nx\ny\n```", "\n\n", "> a\n> b"} {
		got := StripMarkdown(in)
		for _, r := range got {
			if r == '\n' || r == '\r' {
				t.Fatalf("StripMarkdown(%q) = %q contains a newline", in, got)
			}
		}
	}
}
