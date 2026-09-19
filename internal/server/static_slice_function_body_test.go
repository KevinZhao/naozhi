package server

import (
	"testing"
)

// TestSliceFunctionBody_BoundaryCases pins the sliceFunctionBody helper
// itself (R244-CR-P3-1 / #1062) so a regression in the parser can't
// silently false-pass the source-pin tests that slice function bodies
// (static_cron_test.go). Covers:
//
//  1. balanced top-level body returns content up to and including `}\n`;
//  2. nested `{`/`}` blocks are tracked correctly (depth must hit 0);
//  3. unterminated body returns ok=false (caller should t.Fatal);
//  4. closing brace at EOF (no trailing newline) is accepted.
func TestSliceFunctionBody_BoundaryCases(t *testing.T) {
	cases := []struct {
		name   string
		js     string
		idx    int
		want   string
		wantOK bool
	}{
		{
			name:   "simple-balanced",
			js:     "function f() {\n  return 1;\n}\nrest",
			idx:    0,
			want:   "function f() {\n  return 1;\n}\n",
			wantOK: true,
		},
		{
			name:   "nested-braces",
			js:     "fn() {\n  if (x) { y(); }\n  return { a: 1 };\n}\ntail",
			idx:    0,
			want:   "fn() {\n  if (x) { y(); }\n  return { a: 1 };\n}\n",
			wantOK: true,
		},
		{
			name:   "unterminated",
			js:     "function f() {\n  no close",
			idx:    0,
			want:   "",
			wantOK: false,
		},
		{
			name:   "close-at-eof",
			js:     "fn() {\n}",
			idx:    0,
			want:   "fn() {\n}",
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sliceFunctionBody(tc.js, tc.idx)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// sliceFunctionBody slices js[idx:] up to the first `}\n` (function-end
// boundary) that is preceded by a balanced run of inner `{`/`}`. R244-CR-P3-1
// (#1062): replaces magic 4000/2500-byte windows that silently false-passed
// when a function grew larger than the window. Returns ok=false if no closing
// boundary is found, so the caller can t.Fatal instead of asserting against a
// truncated body.
//
// 实现策略: 从 idx 开始线性扫描,跟踪 `{` `}` 计数。第一次出现 `{` 计数从 1
// 起跳;之后每遇 `{` +1,每遇 `}` -1;当计数归零且其后是 `\n` 即为函数结束。
// JS 内字符串/正则/注释里的花括号会被误计入,但 dashboard.js 形态稳定且本检测
// 用于"窗口够不够大",误差只会让 ok=false 触发显式 fatal,不会静默漏断言。
func sliceFunctionBody(js string, idx int) (string, bool) {
	depth := 0
	started := false
	for i := idx; i < len(js); i++ {
		c := js[i]
		switch c {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth == 0 {
				if i+1 < len(js) && js[i+1] == '\n' {
					return js[idx : i+2], true
				}
				// allow EOF immediately after closing brace
				if i+1 == len(js) {
					return js[idx : i+1], true
				}
			}
		}
	}
	return "", false
}
