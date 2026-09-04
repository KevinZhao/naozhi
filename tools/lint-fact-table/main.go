// Command lint-fact-table validates that bold-token facts in design docs
// match the fact-table speech 表（HTML 注释包围的 markdown 表格）.
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
//	<!-- fact-table:end -->
//
// 正文粗体 token 按最近上下文匹配 speech 表 key，与 value 列对账；
// `**-77%** <!-- lint:allow:derived-percentage -->` 白名单跳过。
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
	Source  string
}

// factTableStart / factTableEnd identify the speech 表 boundary.
var (
	factTableStart = regexp.MustCompile(`<!--\s*fact-table:start(?:\s+name="([^"]*)")?\s*-->`)
	factTableEnd   = regexp.MustCompile(`<!--\s*fact-table:end\s*-->`)
	// boldRE captures **...** and its inner content.
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

// lintFile parses the speech 表, scans the body for bold tokens and
// lint:allow comments, and emits token_drift / no_anchor / missing_table.
func lintFile(path string) ([]Violation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(data)

	table, tableErr := parseFactTable(text, path)
	if tableErr != nil {
		// 文档无 speech 表 → warning only。
		return []Violation{{
			Rule:    "missing_table",
			File:    path,
			Message: "no fact-table:start/end markers found; design doc missing single-truth-source table (RFC docs/rfc/lint-fact-table.md §3.1)",
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
		// 纯描述性粗体 (e.g. **必须** / **关键**) 不参与对账。
		if !looksLikeFactValue(tok.text) {
			continue
		}
		key := nearestKeyContext(body, tok.byteOffset)
		matchedKey, expectedValue, ok := matchKey(table, key, tok.text)
		if !ok {
			vs = append(vs, Violation{
				Rule:    "no_anchor",
				File:    path,
				Line:    tok.line,
				Token:   tok.text,
				Message: fmt.Sprintf("bold token %q has no matching fact-table entry; add to fact-table or annotate <!-- lint:allow:<reason> -->", tok.text),
			})
			continue
		}
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

// parseFactTable parses the markdown table between the fact-table markers.
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

	name := ""
	if startMatch[2] >= 0 {
		name = text[startMatch[2]:startMatch[3]]
	}

	tbl := &FactTable{
		Name:    name,
		Entries: make(map[string]string),
		Source:  source,
	}

	lines := strings.Split(tableText, "\n")
	rowsSeen := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}
		if strings.Contains(line, "---") {
			continue
		}
		rowsSeen++
		if rowsSeen == 1 {
			continue
		}
		cols := splitMarkdownRow(line)
		if len(cols) < 2 {
			continue
		}
		key := strings.TrimSpace(cols[0])
		val := strings.TrimSpace(cols[1])
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

// extractBodyOutsideTable returns text minus the speech 表 content.
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

// scanBoldTokens finds all **...** sequences in body.
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

// looksLikeFactValue reports whether token signals a quantitative claim
// (≤ / ≥ or a short digit-bearing string). Precision over recall: version
// numbers, Phase / § / # refs and review item ids (N1 / L1 / M2) are skipped.
func looksLikeFactValue(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "v0.") || strings.HasPrefix(t, "v1.") {
		return false
	}
	if strings.HasPrefix(t, "Phase ") || strings.HasPrefix(t, "§") || strings.HasPrefix(t, "#") {
		return false
	}
	if len(t) <= 4 && (t[0] == 'N' || t[0] == 'L' || t[0] == 'M' || t[0] == 'A') {
		return false
	}
	if strings.ContainsAny(t, "≤≥") {
		return true
	}
	if strings.ContainsAny(t, "0123456789") && len([]rune(t)) <= 20 {
		return true
	}
	return false
}

// nearestKeyContext returns the text since the nearest paragraph / bold /
// sentence boundary before byteOffset; a wider lookback lets a previous
// paragraph's key match this paragraph's token.
func nearestKeyContext(body string, byteOffset int) string {
	start := byteOffset - 100
	if start < 0 {
		start = 0
	}
	chunk := body[start:byteOffset]
	for _, sep := range []string{"\n\n", "**", "。", "\n"} {
		if idx := strings.LastIndex(chunk, sep); idx >= 0 {
			chunk = chunk[idx+len(sep):]
		}
	}
	return chunk
}

// matchKey returns the fact-table entry whose key best matches context.
// A key needs ≥2 tokens present, or one token of length ≥7: a lone "字段"
// hit lacks discriminating power and would match any digit-bearing bold.
func matchKey(table *FactTable, context, tokenValue string) (key, value string, ok bool) {
	type candidate struct {
		key, val      string
		score         int
		matchedTokens int
	}
	var best candidate
	for k, v := range table.Entries {
		score, matched := keyScoreDetail(context, k)
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

// tokenizeKey splits "Server struct 字段" → ["Server", "struct", "字段"].
// Whitespace-only: keyScoreDetail does substring containment per token.
func tokenizeKey(key string) []string {
	return strings.Fields(key)
}

// normalizeKey lowers + trims for case-insensitive matching.
func normalizeKey(k string) string {
	return strings.TrimSpace(k)
}

// valuesEqual compares two value tokens after normValue.
func valuesEqual(a, b string) bool {
	return normValue(a) == normValue(b)
}

// normValue strips bold markers, whitespace and descriptive units.
func normValue(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "**")
	s = strings.TrimSuffix(s, "**")
	s = strings.TrimSpace(s)
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
