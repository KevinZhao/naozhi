package leakguard

import "testing"

func TestDetect_Boundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		leak bool
	}{
		// --- genuine leaks: must be caught ---
		{
			name: "call marker then invoke with parameters",
			text: "先读取它的完整范围。\n\ncall\n<invoke name=\"Read\">\n<parameter name=\"file_path\">/tmp/x.go</parameter>\n</invoke>",
			leak: true,
		},
		{
			name: "self-closing invoke no parameters (ExitPlanMode shape)",
			text: "让我先退出计划模式。\n\ncall\n<invoke name=\"ExitPlanMode\">\n</invoke>",
			leak: true,
		},
		{
			name: "function_calls wrapper marker",
			text: "running now.\n<function_calls>\n<invoke name=\"Bash\">\n<parameter name=\"command\">ls</parameter>\n</invoke>\n</function_calls>",
			leak: true,
		},
		{
			name: "leak at very start of message",
			text: "call\n<invoke name=\"Bash\">\n<parameter name=\"command\">echo hi</parameter>\n</invoke>",
			leak: true,
		},

		// --- must NOT be flagged: quoted syntax in legitimate prose ---
		{
			name: "backtick-quoted invoke in a bug report",
			text: "截图里显示的是 `<invoke name=\"Bash\">` 这种 XML 字面量，夹在文本气泡里。",
			leak: false,
		},
		{
			name: "the word call in prose followed later by quoted invoke",
			text: "I will call the tool. The syntax is `<invoke name=\"X\">` which closes with `</invoke>`.",
			leak: false,
		},
		{
			name: "bare invoke quoted with close tag but no call-line anchor",
			text: "compare `<invoke name=\"a\">x</invoke>` against the structured form.",
			leak: false,
		},
		{
			name: "plain prose no tool syntax",
			text: "这是一条完全正常的回复，讨论了 transcript 渲染与边界问题。",
			leak: false,
		},
		{
			name: "empty",
			text: "",
			leak: false,
		},
		{
			name: "unclosed invoke (truncated turn) is not a leak",
			text: "call\n<invoke name=\"Bash\">\n<parameter name=\"command\">echo hi",
			leak: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Detect(tc.text); got != tc.leak {
				t.Errorf("Detect(%q) = %v, want %v", tc.text, got, tc.leak)
			}
		})
	}
}

func TestStrip_SplitsProseAndLeak(t *testing.T) {
	t.Parallel()
	text := "先读取它的完整范围。\n\ncall\n<invoke name=\"Read\">\n<parameter name=\"file_path\">/tmp/x.go</parameter>\n</invoke>"
	prose, leaked, found := Strip(text)
	if !found {
		t.Fatal("Strip found=false, want true")
	}
	if prose != "先读取它的完整范围。" {
		t.Errorf("prose = %q, want %q", prose, "先读取它的完整范围。")
	}
	if leaked == "" || leaked[:4] != "call" {
		t.Errorf("leaked should start with the call marker, got %q", leaked)
	}
	if Detect(prose) {
		t.Error("stripped prose must not itself be detected as a leak")
	}
}

func TestStrip_MultiInvokeChain(t *testing.T) {
	t.Parallel()
	// Two chained invokes under one function_calls wrapper collapse into one
	// leaked region that ends at the trailing </function_calls>.
	text := "doing two things.\n<function_calls>\n<invoke name=\"Read\">\n</invoke>\n<invoke name=\"Bash\">\n<parameter name=\"command\">ls</parameter>\n</invoke>\n</function_calls>"
	prose, leaked, found := Strip(text)
	if !found {
		t.Fatal("Strip found=false, want true")
	}
	if prose != "doing two things." {
		t.Errorf("prose = %q, want %q", prose, "doing two things.")
	}
	if want := "</function_calls>"; leaked[len(leaked)-len(want):] != want {
		t.Errorf("leaked should end at </function_calls>, got tail %q", leaked[len(leaked)-len(want):])
	}
}

func TestStrip_NoLeak(t *testing.T) {
	t.Parallel()
	if _, _, found := Strip("just normal prose, nothing to see"); found {
		t.Error("Strip found=true on clean prose, want false")
	}
}
