// Command check-router-fields enforces that every field of the
// internal/session.Router struct carries an accurate `// 读写: <files>`
// annotation listing which router_*.go files read or write it.
//
// AST 解析每个字段注释声明的域，再扫描所有 router_*.go 实际的 `r.<field>`
// 访问，把实测域与声明域对账：
//
//   - drift_omitted：字段被某 router_*.go 域 D 访问，但其注释未声明 D。
//   - missing_annotation：字段无 `// 读写:` 注释。
//
// 域名 = router_*.go 文件名去掉 `router_` 前缀与 `.go` 后缀。注释里的非域
// token（"all router_*.go"、"test helpers"、方法名说明）识别为 wildcard，
// 抑制该字段的 drift_omitted。facet 迁到独立包后（如 workspacestore.Store）
// 其 inner 字段由编译器接管，只对账 Router 外层字段；同包 sub-struct facet
// 按 one-level 递归对账。mode=warn（默认）只打 stderr，mode=fail 违规 exit 1。
//
// 用法（从 repo root 运行）：
//
//	check-router-fields [-mode warn|fail] [-dir internal/session]
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
	wildcard bool            // non-domain token seen (all/test/method-name) → suppress drift
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

// check parses Router fields + `// 读写:` annotations from router_core.go
// (recursing one level into same-package sub-struct facets), scans every
// non-test router_*.go for r.<field> / r.<sub>.<inner> accesses, and diffs
// observed domains against declared ones.
func check(dir string) ([]Violation, error) {
	files, err := routerFiles(dir)
	if err != nil {
		return nil, err
	}

	// Named struct types across router_*.go, for one-level facet recursion.
	structDecls, err := collectStructDecls(files)
	if err != nil {
		return nil, fmt.Errorf("collect struct decls: %w", err)
	}

	corePath := filepath.Join(dir, "router_core.go")
	fields, subStructFields, err := parseRouterFields(corePath, structDecls)
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
		hits, err := scanFieldAccess(path, fieldNames, subStructFields)
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
		// Wildcard annotations cover unspecified files — skip drift checks.
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

// collectStructDecls returns typeName → *ast.StructType for every named
// struct declared in files (comments parsed so inner `// 读写:` survive).
func collectStructDecls(files []string) (map[string]*ast.StructType, error) {
	decls := make(map[string]*ast.StructType)
	for _, path := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				decls[ts.Name.Name] = st
			}
			return true
		})
	}
	return decls, nil
}

// parseRouterFields returns one fieldAnnotation per named Router field
// (embedded fields skipped). A field whose type is a same-package struct in
// structDecls is recursed ONE level so inner fields keep their own drift
// accounting; subStructFields maps outer name → inner field-name set.
func parseRouterFields(corePath string, structDecls map[string]*ast.StructType) ([]fieldAnnotation, map[string]map[string]bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, corePath, nil, parser.ParseComments)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("type Router struct not found")
	}

	var out []fieldAnnotation
	subStructFields := make(map[string]map[string]bool)
	for _, field := range structType.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		anno := parseAnnotation(field.Doc)
		for _, id := range field.Names {
			fa := anno
			fa.name = id.Name
			out = append(out, fa)

			// Recurse one level into a named local sub-struct type.
			if typeName, ok := namedTypeIdent(field.Type); ok {
				if inner, ok := structDecls[typeName]; ok {
					innerFields := parseInnerFields(inner)
					if len(innerFields) > 0 {
						names := make(map[string]bool, len(innerFields))
						for _, ifa := range innerFields {
							names[ifa.name] = true
							out = append(out, ifa)
						}
						subStructFields[id.Name] = names
					}
				}
			}
		}
	}
	return out, subStructFields, nil
}

// namedTypeIdent reports whether expr is a bare named type identifier and
// returns its name. Pointer/slice/map/qualified types are not followed: a
// qualified facet type lives in its own package and the compiler already
// forbids r.<outer>.<inner> access there (#2495).
func namedTypeIdent(expr ast.Expr) (string, bool) {
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name, true
	}
	return "", false
}

// parseInnerFields returns one fieldAnnotation per named field of st.
func parseInnerFields(st *ast.StructType) []fieldAnnotation {
	var out []fieldAnnotation
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		anno := parseAnnotation(field.Doc)
		for _, id := range field.Names {
			fa := anno
			fa.name = id.Name
			out = append(out, fa)
		}
	}
	return out
}

// knownDomains is the closed set of tokens that map to a real router_*.go
// file; any other leading token is treated as a wildcard.
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

// parseAnnotation finds the `// 读写: …` line in doc and parses the
// comma-separated domain list (leading token of each segment).
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

// parseDomainList records the leading token of each comma segment: known
// domains go into fa.domains, anything else flags fa.wildcard.
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
		fa.wildcard = true
	}
}

// leadingToken extracts the first identifier-like word of a segment.
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

// scanFieldAccess returns the Router field names referenced in path via a
// `r.<field>` selector. A chained `r.<outer>.<inner>` whose outer is in
// subStructFields credits the inner field, so drift accounting survives
// a field group moving into a facet.
func scanFieldAccess(path string, fieldNames map[string]bool, subStructFields map[string]map[string]bool) (map[string]bool, error) {
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
		// Chained selector r.<outer>.<inner>: credit the inner field.
		if outerSel, ok := sel.X.(*ast.SelectorExpr); ok {
			if base, ok := outerSel.X.(*ast.Ident); ok && base.Name == "r" {
				if inner, ok := subStructFields[outerSel.Sel.Name]; ok && inner[sel.Sel.Name] {
					hits[sel.Sel.Name] = true
				}
			}
			return true
		}
		// Only selectors off the bare "r" receiver count, so unrelated
		// x.field selectors sharing a field name are ignored.
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
