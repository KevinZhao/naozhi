// rule_iface_match (rule 4) — server-split-phase4-design v0.6.1 §四.1.3 / §6.2.0.4.
//
// Phase 0b 交付骨架；Phase 1 PR merge 前必完整实装（设计稿 §6.2.0.4
// 阶段化交付表）。
//
// Rule 4 检查实现侧 godoc 中的 "satisfies: <pkg>.<Iface>" 反向注释——
// docs/design/server-consumer-contracts.md 列出的每个跨包接口必须有
// 对应的 satisfies-by 注释。两者漂移即 fail。
//
// Phase 0b 骨架仅扫 satisfies: 注释存在性 + 列出未声明的实现侧 type；
// Phase 1 前补全：解析 consumer-contracts.md 的方法集 + AST 验证实现
// 侧 type 真的实现了完整方法集（var _ binding 已经做了一半，rule 4
// 是文档侧对账）。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// satisfiesRE matches godoc lines like:
//
//	// satisfies: server.MessageEnqueuer
//	// satisfies-by: *dispatch.MessageQueue (internal/dispatch/msgqueue.go)
//	// satisfies: wshub.MessageEnqueuer (see docs/design/server-consumer-contracts.md#message-queue)
//
// The regex captures the interface ref as group 1; whatever follows in
// parens is just provenance and not validated by rule 4 (let the linker /
// var _ catch implementation drift).
var satisfiesRE = regexp.MustCompile(`(?m)^//\s*satisfies(?:-by)?:\s*([\w./*]+)`)

// scanIfaceMatch (rule 4 stub) walks .go files under root and reports
// godoc satisfies: declarations whose interface ref does not appear in
// docs/design/server-consumer-contracts.md.
//
// Phase 0b: emits warnings only (mode=warn), since consumer-contracts.md
// is a moving target during Phase 1+ ramp-up.
//
// Phase 1 前升级目标：
//  1. 解析 consumer-contracts.md 的接口定义表（方法集 + 跨方法时序契约）
//  2. AST 验证 satisfies-by 注释中的 type 实现完整方法集
//  3. 比对实现侧 godoc 中的 contract id（CT-MQ-1 等）与 consumer-contracts.md 的锚点
func scanIfaceMatch(roots []string) []Violation {
	var out []Violation

	contractsPath := "docs/design/server-consumer-contracts.md"
	contractsData, err := os.ReadFile(contractsPath)
	if err != nil {
		// 文档不存在不是 fail——可能 Phase 0b 之前没建。仅 warn。
		out = append(out, Violation{
			Rule:    "iface_match",
			File:    contractsPath,
			Message: fmt.Sprintf("consumer-contracts.md not readable (%v); rule 4 cannot validate satisfies declarations", err),
		})
		return out
	}
	contracts := string(contractsData)

	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			matches := satisfiesRE.FindAllStringSubmatch(string(data), -1)
			for _, m := range matches {
				if len(m) < 2 {
					continue
				}
				ifaceRef := strings.TrimSpace(m[1])
				// Strip leading * if present (we want type name, not pointer).
				ifaceRef = strings.TrimPrefix(ifaceRef, "*")
				// Last segment after . — this is the interface name.
				parts := strings.Split(ifaceRef, ".")
				ifaceName := parts[len(parts)-1]
				// Phase 0b heuristic: contracts.md must mention the
				// interface name somewhere (anchor, header, or method
				// list line). Phase 1 升级解析每个接口的完整方法集。
				if !strings.Contains(contracts, ifaceName) {
					out = append(out, Violation{
						Rule:    "iface_match",
						File:    filepath.ToSlash(path),
						Message: fmt.Sprintf("godoc declares 'satisfies: %s' but consumer-contracts.md has no entry for %q (Phase 0b stub heuristic; Phase 1 前升级 method-set 完整对账)", m[1], ifaceName),
					})
				}
			}
			return nil
		})
	}
	return out
}
