package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CDNScriptsHaveSRI 锁定 R219-SEC-4：
// dashboard.js 通过动态 <script> / <link> 注入的 KaTeX 与 Mermaid 资产
// 必须带 SRI integrity 哈希 + crossOrigin='anonymous'，否则 CDN 被劫持时
// 无法被浏览器拦截。本测试在 loadKatex / loadMermaid 函数体窗口内
// 各扫描一次 'integrity' 字面量与 'sha384-' 前缀，防止未来 patch 静默
// 把 SRI 移除（任一行被注释 / 删除即触发回归）。
func TestDashboardJS_CDNScriptsHaveSRI(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	cases := []struct {
		fn        string
		minSRI    int // 期望的 integrity 出现次数下限（KaTeX 注入 link+script，Mermaid 注入 script）
		minOrigin int // 期望的 crossOrigin='anonymous' 出现次数下限
	}{
		{"function loadKatex()", 2, 2},   // CSS link + JS script
		{"function loadMermaid()", 1, 1}, // 只有 JS script
	}

	for _, tc := range cases {
		idx := strings.Index(js, tc.fn)
		if idx < 0 {
			t.Errorf("%s: not found in dashboard.js", tc.fn)
			continue
		}
		rest := js[idx:]
		end := strings.Index(rest[1:], "\nfunction ")
		if end < 0 {
			end = len(rest)
		}
		body := rest[:end]

		gotSRI := strings.Count(body, "integrity =") + strings.Count(body, "integrity=")
		if gotSRI < tc.minSRI {
			t.Errorf("%s: integrity attribute count = %d, want >= %d", tc.fn, gotSRI, tc.minSRI)
		}
		gotSHA := strings.Count(body, "'sha384-") + strings.Count(body, "\"sha384-")
		if gotSHA < tc.minSRI {
			t.Errorf("%s: sha384 hash count = %d, want >= %d", tc.fn, gotSHA, tc.minSRI)
		}
		gotOrigin := strings.Count(body, "'anonymous'") + strings.Count(body, "\"anonymous\"")
		if gotOrigin < tc.minOrigin {
			t.Errorf("%s: crossOrigin='anonymous' count = %d, want >= %d", tc.fn, gotOrigin, tc.minOrigin)
		}
	}
}
