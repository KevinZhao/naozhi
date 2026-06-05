// Command check-router-fields enforces that every field of the
// internal/session.Router struct carries an accurate `// 读写: <files>`
// annotation listing which router_*.go files read or write it.
//
// 背景（router_core.go NEEDS-DESIGN R245-ARCH-48）：router-split refactor 把
// Router 的方法拆到多个 router_*.go 文件，每个字段头上手工维护一行
//
//	// 读写: core (init), lifecycle (spawn), cleanup (remove)
//
// 列出哪些 router_*.go 域访问该字段。这是拆分后"字段-方法耦合"唯一可见的
// 机制，但纯手工维护会悄悄腐烂：重构把某 getter 挪进新 router_*.go 却忘了
// 更新注释。本工具是机器兜底——AST 解析每个字段的注释声明域，再 AST 扫描
// 所有 router_*.go 实际的 `r.<field>` 访问，把实测域与声明域对账，发现漂移
// 即报。
//
// 实现遵循 tools/lint-fact-table + tools/lint-server-handlers 既有先例：
//
//   - mode=warn（默认）：违规打到 stderr，exit 0。CI 在 router-split 进行
//     期间用此模式——既有注释漂移先以 warning 暴露、单独 PR 修复，不卡 PR。
//   - mode=fail：任意违规 exit 1。后续稳定后切换的验收门。
//
// 漂移规则：
//
//   - drift_omitted：字段被某 router_*.go 域 D 访问，但其注释未声明 D。
//   - missing_annotation：字段无 `// 读写:` 注释。
//
// 域名 = router_*.go 文件名去掉 `router_` 前缀与 `.go` 后缀（如
// router_capacity.go → capacity）。注释里的非域 token（如 "all router_*.go"
// 这类通配、"test helpers"、以及 "Resolver()" / "wrapperFor" 这类方法名说明）
// 被识别为 wildcard，会抑制该字段的 drift_omitted——它们表达"任意/方法级
// 访问"，无法精确对账到单一文件域。
//
// 纯增量（pure-additive）：本工具只读源码，不改 Router 结构、方法签名、锁
// 拓扑，也不改既有 52 条 `// 读写:` 注释。
//
// 用法：
//
//	check-router-fields [-mode warn|fail] [-dir internal/session]
//
// 从 repo root 运行。
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type mode int

const (
	modeWarn mode = iota
	modeFail
)

// Violation is a single annotation-drift finding.
type Violation struct {
	Rule    string // "drift_omitted" | "missing_annotation"
	Field   string
	Domain  string // the accessing domain that drifted (empty for missing_annotation)
	Message string
}

// fieldAnnotation captures one Router struct field and its parsed `// 读写:`
// declaration.
type fieldAnnotation struct {
	name     string
	hasAnno  bool            // true when a `// 读写:` comment was found
	domains  map[string]bool // declared file-domain set (e.g. {"core","lifecycle"})
	wildcard bool            // annotation carried a non-domain token (all/test/method-name) → suppress drift
}

func main() {
	var (
		runMode = flag.String("mode", "warn", "warn | fail")
		dir     = flag.String("dir", filepath.Join("internal", "session"), "directory holding router_*.go")
	)
	flag.Parse()

	m := modeWarn
	if *runMode == "fail" {
		m = modeFail
	}

	vs, err := check(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-router-fields: %v\n", err)
		os.Exit(2)
	}

	emitText(vs)

	if len(vs) > 0 && m == modeFail {
		os.Exit(1)
	}
}

// check runs the full pipeline against the router_*.go files in dir:
//  1. parse router_core.go, extract Router fields + their `// 读写:` annotations
//  2. AST-scan every non-test router_*.go for r.<field> accesses, mapping each
//     hit to the file's domain
//  3. compare observed domains against declared domains; emit Violations
func check(dir string) ([]Violation, error) {
	files, err := routerFiles(dir)
	if err != nil {
		return nil, err
	}

	corePath := filepath.Join(dir, "router_core.go")
	fields, err := parseRouterFields(corePath)
	if err != nil {
		return nil, fmt.Errorf("parse Router struct: %w", err)
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("no Router struct fields found in %s", corePath)
	}

	fieldNames := make(map[string]bool, len(fields))
	for _, f := range fields {
		fieldNames[f.name] = true
	}

	// observed[field] = set of domains that actually access it.
	observed := make(map[string]map[string]bool)
	for _, path := range files {
		domain := fileDomain(path)
		hits, err := scanFieldAccess(path, fieldNames)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", path, err)
		}
		for f := range hits {
			if observed[f] == nil {
				observed[f] = make(map[string]bool)
			}
			observed[f][domain] = true
		}
	}

	return diff(fields, observed), nil
}

// diff compares each field's declared domains against the observed access set.
func diff(fields []fieldAnnotation, observed map[string]map[string]bool) []Violation {
	var vs []Violation
	for _, f := range fields {
		if !f.hasAnno {
			vs = append(vs, Violation{
				Rule:    "missing_annotation",
				Field:   f.name,
				Message: fmt.Sprintf("field %q has no `// 读写:` annotation (router_core.go maintenance rule: every Router field must declare its access set)", f.name),
			})
			continue
		}
		// Wildcard annotations (all router_*.go / test helpers / method-name
		// notes) intentionally cover unspecified files — skip drift checks.
		if f.wildcard {
			continue
		}
		for domain := range observed[f.name] {
			if !f.domains[domain] {
				vs = append(vs, Violation{
					Rule:    "drift_omitted",
					Field:   f.name,
					Domain:  domain,
					Message: fmt.Sprintf("field %q is accessed in router_%s.go but its `// 读写:` annotation omits %q", f.name, domain, domain),
				})
			}
		}
	}
	return vs
}

// routerFiles returns the non-test router_*.go paths in dir, sorted.
func routerFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if !strings.HasPrefix(name, "router_") || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no router_*.go files in %s", dir)
	}
	return out, nil
}

// fileDomain maps "…/router_capacity.go" → "capacity".
func fileDomain(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".go")
	base = strings.TrimPrefix(base, "router_")
	return base
}

// parseRouterFields parses corePath, finds `type Router struct`, and returns
// one fieldAnnotation per named field (embedded fields are skipped).
func parseRouterFields(corePath string) ([]fieldAnnotation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, corePath, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var structType *ast.StructType
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Router" {
			return true
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			structType = st
			return false
		}
		return true
	})
	if structType == nil {
		return nil, fmt.Errorf("type Router struct not found")
	}

	var out []fieldAnnotation
	for _, field := range structType.Fields.List {
		// Skip embedded fields (no names).
		if len(field.Names) == 0 {
			continue
		}
		anno := parseAnnotation(field.Doc)
		// One annotation block applies to all names on the same field decl
		// (rare for this struct, but handle it).
		for _, id := range field.Names {
			fa := anno
			fa.name = id.Name
			out = append(out, fa)
		}
	}
	return out, nil
}

// knownDomains is the closed set of file-domain tokens that map to a real
// router_*.go file. A leading comma-segment token in this set is a declared
// domain; any other leading token is treated as a wildcard (method-name note
// like "Resolver()" / "wrapperFor", or "all"/"test" coverage phrases).
var knownDomains = map[string]bool{
	"core":          true,
	"lifecycle":     true,
	"lifecycle_log": true,
	"cleanup":       true,
	"discovery":     true,
	"shim":          true,
	"backend":       true,
	"capacity":      true,
	"workspace":     true,
}

// parseAnnotation scans a field's doc comment group for a `// 读写: …` line
// and parses the comma-separated domain list. The annotation may span the
// comment text after the marker; only the leading token of each comma segment
// is interpreted as a domain.
func parseAnnotation(doc *ast.CommentGroup) fieldAnnotation {
	fa := fieldAnnotation{domains: map[string]bool{}}
	if doc == nil {
		return fa
	}
	const marker = "读写:"
	for _, c := range doc.List {
		text := strings.TrimPrefix(c.Text, "//")
		text = strings.TrimSpace(text)
		idx := strings.Index(text, marker)
		if idx < 0 {
			continue
		}
		fa.hasAnno = true
		body := text[idx+len(marker):]
		parseDomainList(&fa, body)
	}
	return fa
}

// parseDomainList splits the annotation body on commas and records the leading
// token of each segment. Recognized tokens go into fa.domains; an unrecognized
// leading token (method name, "all", "test", …) flags fa.wildcard.
func parseDomainList(fa *fieldAnnotation, body string) {
	for _, seg := range strings.Split(body, ",") {
		tok := leadingToken(seg)
		if tok == "" {
			continue
		}
		if knownDomains[tok] {
			fa.domains[tok] = true
			continue
		}
		// Non-domain leading token → wildcard coverage we can't pin to a file.
		fa.wildcard = true
	}
}

// leadingToken extracts the first identifier-like word from a segment, e.g.
// " lifecycle (spawn/reset)" → "lifecycle", "all router_*.go (…)" → "all".
func leadingToken(seg string) string {
	seg = strings.TrimSpace(seg)
	end := 0
	for end < len(seg) {
		ch := seg[end]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' {
			end++
			continue
		}
		break
	}
	return seg[:end]
}

// scanFieldAccess AST-walks path and returns the set of Router field names
// referenced via a `r.<field>` selector (any receiver identifier — the
// selector's field name is what matters, and r is the conventional receiver).
func scanFieldAccess(path string, fieldNames map[string]bool) (map[string]bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	hits := make(map[string]bool)
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// Only count selectors off a bare identifier receiver (r.field),
		// matching the Router method convention. This avoids matching
		// unrelated x.field selectors on other types that happen to share
		// a field name.
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "r" {
			return true
		}
		if fieldNames[sel.Sel.Name] {
			hits[sel.Sel.Name] = true
		}
		return true
	})
	return hits, nil
}

func emitText(vs []Violation) {
	if len(vs) == 0 {
		fmt.Fprintln(os.Stderr, "check-router-fields: no violations")
		return
	}
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Field != vs[j].Field {
			return vs[i].Field < vs[j].Field
		}
		if vs[i].Rule != vs[j].Rule {
			return vs[i].Rule < vs[j].Rule
		}
		return vs[i].Domain < vs[j].Domain
	})
	for _, v := range vs {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", v.Rule, v.Message)
	}
	fmt.Fprintf(os.Stderr, "check-router-fields: %d violation(s)\n", len(vs))
}
