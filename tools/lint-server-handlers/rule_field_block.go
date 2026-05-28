// Phase 0b 交付 rule 3a 骨架——server-split-phase4-design v0.6.1 §6.2.0.4 阶段化交付表。
//
// rule 3a (Phase 0 必交付): 仅检查 wshub_*.go 的 godoc 头部含
// "Field-block contract:" / "WRITES:" / "READS-ALSO:" / "LIFECYCLE-METHOD"
// 标注。立刻给"忘了写 godoc 头"的反馈（最常见的疏忽）。
//
// rule 3b (Phase 4b 前): AST 字段访问对账 + 与 §五 7 块字段归属表对账 +
// 跨方法调用追踪——复杂 AST 实装，留 Phase 4b 中段补完。
//
// 本文件只实装 3a，3b 的占位 stub 在 main.go 已有 (skeleton message)。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// scanFieldBlockMarkers (rule 3a) returns one Violation per wshub_*.go file
// missing the godoc field-block marker.
//
// Why text-level marker check, not AST: rule 3a's job is to catch the
// 80% case where a developer adds a method but forgets the godoc header.
// AST字段访问对账（rule 3b）才需要语义分析，留 Phase 4b 前补完。
//
// Markers recognized:
//   - "Field-block contract:" — required in wshub.go (the file that owns
//     the Hub struct definition + 7-block godoc table)
//   - "WRITES:" — required in any wshub_<block>.go method-file declaring
//     which field block its methods write
//   - "READS-ALSO:" — opt-in for cross-block read-only access (§五 跨块
//     只读豁免)
//   - "LIFECYCLE-METHOD" — Shutdown / Start / NewHub style cross-block
//     write豁免 (§五 v0.6.1 lifecycle 块跨块写豁免)
//
// Phase 4b PR 升级 rule 3b 时，额外要求 godoc 头列出的字段块名称必须
// 出现在 §五 7 块归属表里——本 stub 只检查 marker 存在性，不验内容。
func scanFieldBlockMarkers(serverPkg string) []Violation {
	var out []Violation

	entries, err := os.ReadDir(serverPkg)
	if err != nil {
		return out
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		name := e.Name()
		// Phase 0b 仅扫 wshub*.go (前缀)。Phase 4 抽到 internal/wshub/ 后，
		// 调用方改 scanFieldBlockMarkers("internal/wshub") 即可复用。
		if !strings.HasPrefix(name, "wshub") {
			continue
		}

		path := filepath.Join(serverPkg, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		header := extractGodocHeader(string(data))

		// wshub.go: must contain "Field-block contract"
		if name == "wshub.go" {
			if !strings.Contains(header, "Field-block contract") {
				out = append(out, Violation{
					Rule:    "field_block",
					File:    filepath.ToSlash(path),
					Message: "wshub.go missing 'Field-block contract:' godoc header (server-split-phase4-design.md v0.6.1 §五)",
				})
			}
			continue
		}

		// hub_<block>.go: must contain WRITES / READS-ALSO / LIFECYCLE-METHOD
		if hasAnyMarker(header, "WRITES:", "READS-ALSO:", "LIFECYCLE-METHOD") {
			continue
		}
		out = append(out, Violation{
			Rule:    "field_block",
			File:    filepath.ToSlash(path),
			Message: fmt.Sprintf("%s missing field-block godoc marker (WRITES: / READS-ALSO: / LIFECYCLE-METHOD); rule 3a Phase 0b expects markers ahead of Phase 4b strict-mode rule 3b AST 对账 (§五)", name),
		})
	}
	return out
}

// extractGodocHeader returns the first ~200 lines of a Go source file,
// truncated to before the first non-comment statement that isn't a
// package / import / type declaration. This catches both file-level
// godoc (above package decl) AND type-level godoc (above type Hub
// struct), since rule 3a is about declaring intent — not about which
// declaration the comment attaches to.
//
// Phase 0b heuristic; Phase 4b rule 3b will use full AST and attach
// godoc to specific declaration nodes.
func extractGodocHeader(text string) string {
	lines := strings.SplitN(text, "\n", 250)
	var sb strings.Builder
	for i, line := range lines {
		if i >= 200 {
			break
		}
		// Stop scanning once we see a function/method/var declaration
		// that isn't a comment or part of struct definition.
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "func ") {
			// First func decl — stop. Comments above this point are the
			// godoc that rule 3a cares about.
			break
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// hasAnyMarker returns true if text contains any of the provided markers.
func hasAnyMarker(text string, markers ...string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}
