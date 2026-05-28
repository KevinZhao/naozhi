// rule_stale_exemption (rule 5) — server-split-phase4-design v0.6.1 §6.2.0.4 反向依赖保护.
//
// Phase 0b 交付框架（读 exemptions.yaml + 列文件存在性检查）；Phase 1 前
// 完整 git tag 比对（设计稿 §6.2.0.4 Rule 5 阶段化交付表）。
//
// Rule 5 解决 v0.4 缺漏：until_phase 只规定"该豁免在 phase N 之前有效"，
// 但没有强制 phase N merge 后必须删除。如果 Phase 2 merge 了但有人忘了
// 从 exemptions.yaml 删 project_files.go 条目，CI 不会 fail——豁免债越积
// 越多直到 Phase 5 切 fail 模式才崩。
//
// Rule 5 行为：
//  1. 读 exemptions.yaml file_size[].until_phase
//  2. 与 git tag 列表比对：若 server-split-phase{N} tag 已存在，那 N 之
//     前的 until_phase 条目必须不再被引用：
//     a) 文件已不存在 → 条目应删除（fail）
//     b) 文件存在但行数已 ≤ limit → 条目应删除（fail）
//     c) 条目已从 yaml 删除 → 通过
//  3. Phase X PR commit message 必须含 "Closes-exemption: <file>" trailer
//
// Phase 0b 骨架仅做 (a) 文件存在性检查；(b) 行数比对 + (c) git tag 对账
// 留 Phase 1 前补完。
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// scanStaleExemption (rule 5 stub, Phase 0b) checks that every file_size
// exemption entry references a real file. Returns Violations for entries
// that point at non-existent paths.
//
// Phase 1 前升级：
//   - git tag 对账（os/exec 调 `git tag --list 'server-split-phase*'`）
//   - 行数比对（如 file <= limit 但 entry 仍在 → stale）
//   - Closes-exemption commit trailer 校验（CI 集成）
func scanStaleExemption(exempts *exemptions) []Violation {
	var out []Violation
	for _, e := range exempts.FileSize {
		if _, err := os.Stat(e.Path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			continue
		}
		// File doesn't exist — entry is stale.
		out = append(out, Violation{
			Rule: "stale_exemption",
			File: filepath.ToSlash(e.Path),
			Message: fmt.Sprintf("exemption entry references non-existent file %q (until_phase: %s) — entry should be removed; Phase X PR commit message must include 'Closes-exemption: %s'",
				e.Path, e.UntilPhase, e.Path),
		})
	}
	return out
}
