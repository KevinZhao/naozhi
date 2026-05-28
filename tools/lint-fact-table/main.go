// Command lint-fact-table validates that bold-token facts in design docs
// match the fact-table speech 表（HTML 注释包围的 markdown 表格）.
//
// 设计目标：解决 server-split-phase4-design.md v0.6.1 §0 修订纪律 5
// 钉死的痼疾——"长设计稿事实漂移"。v0.5 把 Hub 字段 37→43 改后只同步
// 了 4/9 处。本工具是机器约束兜底。
//
// 用法：
//
//	lint-fact-table [-mode warn|fail] <file.md> [...]
//	lint-fact-table -sarif <file.md> > findings.sarif
//
// speech 表语法（详 docs/rfc/lint-fact-table.md §3）：
//
//	<!-- fact-table:start name="<id>" -->
//	| 维度 | 实测值 | ... |
//	|---|---|---|
//	| Server struct 字段 | **47** | ... |
//	| Hub struct 字段 | **47** | ... |
//	<!-- fact-table:end -->
//
// 正文粗体 token 与 speech 表 value 列对账：
//
//	"Server struct 47 字段" → 找到上下文 "Server" + token "47" →
//	speech 表里 "Server struct 字段 = 47" → 一致 ✓
//
// 白名单：
//
//	**-77%** <!-- lint:allow:derived-percentage -->
//
// Phase 0-LFT 实装范围（v1 RFC §4）：
//   - 单文档 speech 表 + 正文 token 对账（启发式上下文 ≤50 字符）
//   - 白名单标注解析
//   - 3 个 testdata case（valid / invalid_token / missing_table）
//   - CI mode=warn / mode=fail / SARIF 输出
//
// v2 / 后续扩展（不在本 PR 范围）：
//   - 跨文档速查表（baseline.md 与 design.md 对账）
//   - 单位语义解析（13-15 周 vs 13 PR vs 47 字段）
//   - 自动从 baseline 推导速查表
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

type mode int

const (
	modeWarn mode = iota
	modeFail
)

// Violation is a fact-drift finding.
type Violation struct {
	Rule    string // "token_drift" | "missing_table" | "no_anchor"
	File    string
	Line    int
	Token   string
	Message string
}

// FactTable maps key (e.g. "Hub struct 字段") → value token (e.g. "47").
type FactTable struct {
	Name    string
	Entries map[string]string // normalized-key → bolded-value-token
	Source  string            // file path
}

// factTableStart / factTableEnd identify the speech 表 boundary.
var (
	factTableStart = regexp.MustCompile(`<!--\s*fact-table:start(?:\s+name="([^"]*)")?\s*-->`)
	factTableEnd   = regexp.MustCompile(`<!--\s*fact-table:end\s*-->`)
	// boldRE captures any **...** sequence, capturing the inner content.
	boldRE = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	// allowRE matches whitelist comments at end-of-line: <!-- lint:allow:<reason> -->
	allowRE = regexp.MustCompile(`<!--\s*lint:allow:([^\s]+)\s*-->`)
)

func main() {
	var (
		runMode = flag.String("mode", "warn", "warn | fail")
		sarif   = flag.Bool("sarif", false, "emit SARIF on stdout")
		strict  = flag.Bool("strict", false, "report no_anchor warnings (default: only token_drift + missing_table)")
	)
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: lint-fact-table [-mode warn|fail] [-sarif] <file.md> [...]")
		os.Exit(2)
	}

	m := modeWarn
	if *runMode == "fail" {
		m = modeFail
	}

	var allViolations []Violation
	for _, path := range flag.Args() {
		vs, err := lintFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(2)
		}
		// 默认抑制 no_anchor（启发式 false positive 太多）；-strict 才报。
		if !*strict {
			filtered := vs[:0]
			for _, v := range vs {
				if v.Rule != "no_anchor" {
					filtered = append(filtered, v)
				}
			}
			vs = filtered
		}
		allViolations = append(allViolations, vs...)
	}

	if *sarif {
		emitSARIF(allViolations)
	} else {
		emitText(allViolations)
	}

	if len(allViolations) > 0 && m == modeFail {
		os.Exit(1)
	}
}

// lintFile is the per-file pipeline:
//  1. parse speech 表 (boundary + table rows)
//  2. scan body for bold tokens + whitelist comments
//  3. match tokens against speech 表 keys (heuristic: nearest preceding
//     bolded "key" word within 50 chars)
//  4. emit Violations for drifts / missing tables.
func lintFile(path string) ([]Violation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(data)

	table, tableErr := parseFactTable(text, path)
	if tableErr != nil {
		// 文档无 speech 表 → warning only（v1 RFC §3.4）。
		return []Violation{{
			Rule:    "missing_table",
			File:    path,
			Message: fmt.Sprintf("no fact-table:start/end markers found; design doc missing single-truth-source table (RFC docs/rfc/lint-fact-table.md §3.1)"),
		}}, nil
	}

	body := extractBodyOutsideTable(text)
	tokens := scanBoldTokens(body)
	whitelist := scanWhitelist(body)

	var vs []Violation
	for _, tok := range tokens {
		if whitelist[tok.line] {
			continue
		}
		// Heuristic key matching: tokens that look like values (containing
		// digits OR "≤" / "周" / 单位) are checked against table values.
		// 纯描述性粗体 (e.g. **必须** / **关键**) 不参与对账。
		if !looksLikeFactValue(tok.text) {
			continue
		}
		// Find the nearest matching fact-table entry.
		key := nearestKeyContext(body, tok.byteOffset)
		matchedKey, expectedValue, ok := matchKey(table, key, tok.text)
		if !ok {
			// Unable to map token to table entry — emit warning so author
			// can either (a) add to fact-table, or (b) add lint:allow.
			vs = append(vs, Violation{
				Rule:    "no_anchor",
				File:    path,
				Line:    tok.line,
				Token:   tok.text,
				Message: fmt.Sprintf("bold token %q has no matching fact-table entry; add to fact-table or annotate <!-- lint:allow:<reason> -->", tok.text),
			})
			continue
		}
		// Compare token to expected value (normalized).
		if !valuesEqual(tok.text, expectedValue) {
			vs = append(vs, Violation{
				Rule:    "token_drift",
				File:    path,
				Line:    tok.line,
				Token:   tok.text,
				Message: fmt.Sprintf("token %q drifted from fact-table[%q]=%q", tok.text, matchedKey, expectedValue),
			})
		}
	}
	return vs, nil
}

// parseFactTable locates the fact-table:start/end markers and parses the
// markdown table inside. Returns nil if no markers found.
func parseFactTable(text, source string) (*FactTable, error) {
	startMatch := factTableStart.FindStringSubmatchIndex(text)
	if startMatch == nil {
		return nil, fmt.Errorf("no fact-table:start marker")
	}
	endMatch := factTableEnd.FindStringIndex(text[startMatch[1]:])
	if endMatch == nil {
		return nil, fmt.Errorf("fact-table:start without matching end")
	}
	tableText := text[startMatch[1] : startMatch[1]+endMatch[0]]

	// name capture
	name := ""
	if startMatch[2] >= 0 {
		name = text[startMatch[2]:startMatch[3]]
	}

	tbl := &FactTable{
		Name:    name,
		Entries: make(map[string]string),
		Source:  source,
	}

	// Parse table rows. Skip header + separator rows.
	lines := strings.Split(tableText, "\n")
	rowsSeen := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}
		// Separator row: |---|---|...
		if strings.Contains(line, "---") {
			continue
		}
		rowsSeen++
		if rowsSeen == 1 {
			// Header row — skip.
			continue
		}
		cols := splitMarkdownRow(line)
		if len(cols) < 2 {
			continue
		}
		key := strings.TrimSpace(cols[0])
		val := strings.TrimSpace(cols[1])
		// Extract bold token from value column if present.
		if m := boldRE.FindStringSubmatch(val); m != nil {
			val = m[1]
		}
		tbl.Entries[normalizeKey(key)] = val
	}
	return tbl, nil
}

// splitMarkdownRow splits "| a | b | c |" → ["a", "b", "c"].
func splitMarkdownRow(line string) []string {
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

// extractBodyOutsideTable returns the document text minus the speech 表
// content, so token scanning doesn't double-count entries IN the table.
func extractBodyOutsideTable(text string) string {
	startMatch := factTableStart.FindStringIndex(text)
	if startMatch == nil {
		return text
	}
	endMatch := factTableEnd.FindStringIndex(text[startMatch[0]:])
	if endMatch == nil {
		return text
	}
	endAbs := startMatch[0] + endMatch[1]
	if endAbs > len(text) {
		return text[:startMatch[0]]
	}
	return text[:startMatch[0]] + text[endAbs:]
}

// boldToken describes a **bold** occurrence in the body.
type boldToken struct {
	text       string
	line       int
	byteOffset int
}

// scanBoldTokens finds all **...** sequences in the body, returning their
// inner text + line number + byte offset (for context lookup).
func scanBoldTokens(body string) []boldToken {
	var out []boldToken
	matches := boldRE.FindAllStringSubmatchIndex(body, -1)
	for _, m := range matches {
		if m[2] < 0 {
			continue
		}
		txt := body[m[2]:m[3]]
		line := 1 + strings.Count(body[:m[0]], "\n")
		out = append(out, boldToken{text: txt, line: line, byteOffset: m[0]})
	}
	return out
}

// scanWhitelist returns a set of line numbers carrying a lint:allow comment.
func scanWhitelist(body string) map[int]bool {
	out := make(map[int]bool)
	matches := allowRE.FindAllStringIndex(body, -1)
	for _, m := range matches {
		line := 1 + strings.Count(body[:m[0]], "\n")
		out[line] = true
	}
	return out
}

// looksLikeFactValue heuristic: token signals a quantitative claim.
//
// 启用：含 ≤ / ≥ 或 短数字串（≤ 6 字符）+ 单位（行 / 字段 / PR / 周 / 个）
// 抑制：v0.X 版本号、Phase 数字、commit hash、章节号、引用（"§N" / "#N"）
//
// 设计意图：精度 > 召回率——v1 仅 catch 明显的事实声明（"47" / "≤ 12" /
// "21313 行"），不去 lint "Phase 4 范围"/"v0.5 改..." 这种描述性 bold。
// v2 可加单位语义解析提升召回。
func looksLikeFactValue(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return false
	}
	// 抑制：v0.X / v1.X 版本号
	if strings.HasPrefix(t, "v0.") || strings.HasPrefix(t, "v1.") {
		return false
	}
	// 抑制：Phase 标识 / 章节引用 / PR 引用
	if strings.HasPrefix(t, "Phase ") || strings.HasPrefix(t, "§") || strings.HasPrefix(t, "#") {
		return false
	}
	// 抑制：N1/N2/L/M 等 review item 编号（含数字但非事实）
	if len(t) <= 4 && (t[0] == 'N' || t[0] == 'L' || t[0] == 'M' || t[0] == 'A') {
		// 例如 "N1" "L1" "M2"
		return false
	}
	// 启用：含 ≤ / ≥（明确事实声明）
	if strings.ContainsAny(t, "≤≥") {
		return true
	}
	// 启用：含数字 AND 长度 ≤ 20（避免长描述性 bold 含数字误匹配）
	if strings.ContainsAny(t, "0123456789") && len([]rune(t)) <= 20 {
		return true
	}
	return false
}

// nearestKeyContext extracts the nearest key-context line for the token.
// Only looks at characters since the most recent newline or bold close
// marker (`**`)——避免跨段落 / 跨粗体 token 误匹配（v1 实测：80 字符回溯
// 会让前一段落的 "字段" 误匹配本段的 "21313"）。
func nearestKeyContext(body string, byteOffset int) string {
	start := byteOffset - 100
	if start < 0 {
		start = 0
	}
	chunk := body[start:byteOffset]
	// 截断到最近的段落边界（双换行 / 粗体闭合 / 句号）。
	for _, sep := range []string{"\n\n", "**", "。", "\n"} {
		if idx := strings.LastIndex(chunk, sep); idx >= 0 {
			chunk = chunk[idx+len(sep):]
		}
	}
	return chunk
}

// matchKey searches the fact table for an entry whose key appears in the
// context. Returns the matched key + expected value if found.
//
// Phase 0-LFT v1: linear scan with substring-match + minimum score
// threshold to avoid false positives. v2 may upgrade to keyword index.
//
// Minimum score: key 必须有至少 2 个 tokens 在 context 出现（或 1 个
// token 长度 ≥ 4）。Single-char "字段" 匹配 6 字节但缺乏判别力，会让
// "Phase 4 范围" / "Phase 5 后保留" 等含数字的粗体随便匹配。
func matchKey(table *FactTable, context, tokenValue string) (key, value string, ok bool) {
	type candidate struct {
		key, val      string
		score         int
		matchedTokens int
	}
	var best candidate
	for k, v := range table.Entries {
		score, matched := keyScoreDetail(context, k)
		// 最低判别要求：要么 ≥2 个 tokens 命中，要么 1 个 token 长度 ≥7（如
		// "Server"/"Hub struct"/"路由数"）。单字符 "字段" 命中不算。
		hasStrongMatch := matched >= 2 || score >= 7
		if !hasStrongMatch {
			continue
		}
		if score > best.score {
			best = candidate{key: k, val: v, score: score, matchedTokens: matched}
		}
	}
	if best.score == 0 {
		return "", "", false
	}
	return best.key, best.val, true
}

// keyScoreDetail returns (score, matchedTokenCount).
func keyScoreDetail(context, key string) (int, int) {
	ctx := strings.ToLower(context)
	keywords := tokenizeKey(key)
	score := 0
	matched := 0
	for _, kw := range keywords {
		if len(kw) < 2 {
			continue
		}
		if strings.Contains(ctx, strings.ToLower(kw)) {
			score += len(kw)
			matched++
		}
	}
	return score, matched
}

// keyScore returns how strongly the context matches a table key. Higher
// is better. Returns 0 if no key word appears.
func keyScore(context, key string) int {
	ctx := strings.ToLower(context)
	keywords := tokenizeKey(key)
	score := 0
	for _, kw := range keywords {
		if strings.Contains(ctx, strings.ToLower(kw)) {
			score += len(kw)
		}
	}
	return score
}

// tokenizeKey splits "Server struct 字段" → ["Server", "struct", "字段"].
func tokenizeKey(key string) []string {
	parts := strings.Fields(key)
	// Also split CJK chunks at character level for substring match.
	var out []string
	for _, p := range parts {
		out = append(out, p)
	}
	return out
}

// normalizeKey lowers + trims for case-insensitive matching.
func normalizeKey(k string) string {
	return strings.TrimSpace(k)
}

// valuesEqual compares two value tokens with simple normalisation.
// "47", "**47**", "47 字段" all match each other.
func valuesEqual(a, b string) bool {
	return normValue(a) == normValue(b)
}

// normValue strips bold markers + whitespace + common units like "字段" /
// "行" / "个" — comparing the numeric/comparator core.
func normValue(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "**")
	s = strings.TrimSuffix(s, "**")
	s = strings.TrimSpace(s)
	// Strip trailing units that are essentially descriptive.
	for _, u := range []string{" 字段", " 行", " 个", " PR", " 周"} {
		s = strings.TrimSuffix(s, u)
	}
	return strings.TrimSpace(s)
}

func emitText(vs []Violation) {
	if len(vs) == 0 {
		fmt.Fprintln(os.Stderr, "lint-fact-table: no violations")
		return
	}
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].File != vs[j].File {
			return vs[i].File < vs[j].File
		}
		return vs[i].Line < vs[j].Line
	})
	for _, v := range vs {
		if v.Line > 0 {
			fmt.Fprintf(os.Stderr, "%s:%d: [%s] %s\n", v.File, v.Line, v.Rule, v.Message)
		} else {
			fmt.Fprintf(os.Stderr, "%s: [%s] %s\n", v.File, v.Rule, v.Message)
		}
	}
	fmt.Fprintf(os.Stderr, "lint-fact-table: %d violation(s)\n", len(vs))
}

func emitSARIF(vs []Violation) {
	const head = `{"$schema":"https://docs.oasis-open.org/sarif/sarif/v2.1.0/cos02/schemas/sarif-schema-2.1.0.json","version":"2.1.0","runs":[{"tool":{"driver":{"name":"lint-fact-table","informationUri":"https://github.com/naozhi/naozhi/blob/master/docs/rfc/lint-fact-table.md","rules":[{"id":"token_drift"},{"id":"missing_table"},{"id":"no_anchor"}]}},"results":[`
	const tail = `]}]}`
	var sb strings.Builder
	sb.WriteString(head)
	for i, v := range vs {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb,
			`{"ruleId":%q,"level":"warning","message":{"text":%q},"locations":[{"physicalLocation":{"artifactLocation":{"uri":%q},"region":{"startLine":%d}}}]}`,
			v.Rule, v.Message, v.File, max1(v.Line))
	}
	sb.WriteString(tail)
	fmt.Println(sb.String())
}

func max1(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}
