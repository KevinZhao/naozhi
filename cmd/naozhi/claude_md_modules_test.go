package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestClaudeMDModuleList_MatchesInternalPackages 把 CLAUDE.md「Module
// Dependency」清单与 internal/ 顶层目录对账，双向缺一即 fail。
//
// 动机：该清单曾漂移到只覆盖 17/49 个包（textutil 还挂着过期的
// "zero-dependency leaf" 描述），靠 grep 人肉同步必漏——与
// TestGenerateSystemdUnit_MatchesDeployTemplate 同理，用测试把文档
// 钉在磁盘事实上。只对账顶层包名；描述文字与子包列举不在 gate 范围。
func TestClaudeMDModuleList_MatchesInternalPackages(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}

	section := moduleDependencySection(t, string(raw))

	// 清单行形如 "  -> osutil       Home/路径展开…"
	mentionRE := regexp.MustCompile(`(?m)^\s*-> ([a-z][a-z0-9]*)\b`)
	mentioned := make(map[string]bool)
	for _, m := range mentionRE.FindAllStringSubmatch(section, -1) {
		mentioned[m[1]] = true
	}
	if len(mentioned) == 0 {
		t.Fatal("Module Dependency 章节没有解析出任何 \"-> <pkg>\" 行；清单格式变了需同步本测试")
	}

	entries, err := os.ReadDir(filepath.Join("..", "..", "internal"))
	if err != nil {
		t.Fatalf("read internal/: %v", err)
	}
	onDisk := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() {
			onDisk[e.Name()] = true
		}
	}

	for pkg := range onDisk {
		if !mentioned[pkg] {
			t.Errorf("internal/%s 存在但 CLAUDE.md Module Dependency 清单未列出——新增包请补一行", pkg)
		}
	}
	for pkg := range mentioned {
		if !onDisk[pkg] {
			t.Errorf("CLAUDE.md Module Dependency 列出 %q 但 internal/%s 不存在——包已删除或改名，请更新清单", pkg, pkg)
		}
	}
}

// moduleDependencySection 截取 "### Module Dependency" 到下一个 "### " 标题
// 之间的文本。
func moduleDependencySection(t *testing.T, doc string) string {
	t.Helper()
	const heading = "### Module Dependency"
	start := strings.Index(doc, heading)
	if start < 0 {
		t.Fatalf("CLAUDE.md 缺少 %q 标题", heading)
	}
	rest := doc[start+len(heading):]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}
