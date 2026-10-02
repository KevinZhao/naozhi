// anchor-keep: Router's size (fields, methods, who names *Router, what its facets can reach, file lengths) is a property of the declarations, so only the source can show it; the fixtures below prove each rule fires.
package session

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The Router budget (#3023, epic #2897 S12). Every count is strict: above the
// baseline is growth, below it means the PR that shrank Router lowers the
// constant in the same change, so nothing grows back into the slack.
const (
	// routerFieldBaseline counts Router's field names; an embedded field is 1.
	// A type that stands in for Router (see routerTypeRefBaseline) adds its
	// own fields, the embedded Router excluded.
	routerFieldBaseline = 33
	// routerMethodBaseline counts methods whose receiver is Router, *Router
	// or a type that stands in for Router.
	routerMethodBaseline = 118
	// routerTypeRefBaseline counts the identifier Router outside method
	// receivers and its own declaration: parameters, results, fields,
	// aliases, conversions, composite literals. A package func taking *Router
	// or a routerOps{r *Router} wrapper would each slip past the method count;
	// they land here. A type that stands in for Router is Router under
	// another spelling: an alias that mentions it (`type R = Router`,
	// `type RP = *Router`), a defined type over it (`type X Router`, which
	// has its fields) and a struct that embeds it (`struct{ *Router }`,
	// which has its fields and methods), chains included. Every use of one
	// counts here too, and its methods count as Router methods.
	routerTypeRefBaseline = 5
)

// Every non-test file in the package root stays within
// routerFileLinesTargetBaseline lines. The two files above it carry an
// exemption that may only shrink; a file that sinks more than
// routerLinesSlackBaseline below its exemption re-samples it, and one back
// under the limit drops its exemption. routerLineExemptionsBaseline counts
// the exemptions, so a third one is a raise the ratchet ledger sees, however
// it is spelled.
const routerFileLinesTargetBaseline = 900

const routerCoreLinesBaseline = 1252

const routerLifecycleLinesBaseline = 1523

const routerLineExemptionsBaseline = 2

const routerLinesSlackBaseline = 30

// S12's end state. Asserted against measured values once the split lands
// (S12g); until then they are only reported.
const routerFieldTargetBaseline = 22

const routerMethodTargetBaseline = 100

// routerForbidden are the session-table names a facet may not mention: a
// type reachable from Router's other fields that names one of them can reach
// the table without going through Router's transactions.
var routerForbidden = map[string]bool{
	"Router": true, "sessTx": true, "sessView": true, "routerState": true, "routerStateView": true,
}

const sessiontableImport = "github.com/naozhi/naozhi/internal/session/sessiontable"

// routerMeasure is what the budget reads from one parse of the package.
type routerMeasure struct {
	files       int
	structFound bool
	fields      []string
	methods     []string // "file:line Name"
	refs        []string // positions of Router outside receivers
	closure     []string // session types reachable from Router's fields, ss excluded
	isolation   []string // forbidden names inside the closure
	lines       map[string]int
}

// routerLimits is what a measure is held to; the real test passes the
// constants above, fixtures pass their own.
type routerLimits struct {
	fields, methods, refs int
	lineLimit             int
	exempt                map[string]int
	exemptions            int // how many entries exempt may hold
	// floors against a parse that has gone blind
	minFiles, minMethods, minClosure int
}

func realRouterLimits() routerLimits {
	return routerLimits{
		fields: routerFieldBaseline, methods: routerMethodBaseline, refs: routerTypeRefBaseline,
		lineLimit: routerFileLinesTargetBaseline, exemptions: routerLineExemptionsBaseline,
		exempt: map[string]int{
			"router_core.go":      routerCoreLinesBaseline,
			"router_lifecycle.go": routerLifecycleLinesBaseline,
		},
		minFiles: 40, minMethods: 50, minClosure: 5,
	}
}

// lineCount counts lines the way lint-server-handlers' file_size does.
func lineCount(src []byte) int {
	n := strings.Count(string(src), "\n")
	if len(src) > 0 && src[len(src)-1] != '\n' {
		n++
	}
	return n
}

// readRouterSources reads every non-test .go file in the package root.
func readRouterSources(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		srcs[p] = string(b)
	}
	return srcs
}

func parseRouterSources(t *testing.T, srcs map[string]string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for name, src := range srcs {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	return fset, files
}

// baseType strips pointers, parentheses and type arguments from a type
// expression: what is left of *T[P] is T, of *pkg.T is pkg.T.
func baseType(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		default:
			return e
		}
	}
}

// recvBase is the type name a receiver or embedded field names: T, *T, T[P],
// *T[P]; "" for a qualified or unnamed type.
func recvBase(e ast.Expr) string {
	if id, ok := baseType(e).(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// tableImportName is the name file f imports sessiontable under, or "".
func tableImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p != sessiontableImport {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "sessiontable"
	}
	return ""
}

// typeRefs walks a type expression and reports each identifier that names a
// type and each package-qualified type. Field and method names, and the
// selected name of a qualified type, are not references.
func typeRefs(e ast.Node, ident func(*ast.Ident), qualified func(pkg *ast.Ident, sel *ast.Ident)) {
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Field:
			typeRefs(x.Type, ident, qualified)
			return false
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok {
				qualified(pkg, x.Sel)
				return false
			}
			typeRefs(x.X, ident, qualified)
			return false
		case *ast.Ident:
			ident(x)
		}
		return true
	})
}

func measureRouter(fset *token.FileSet, files map[string]*ast.File, lines map[string]int) routerMeasure {
	m := routerMeasure{files: len(files), lines: lines}
	type decl struct {
		spec *ast.TypeSpec
		file *ast.File
	}
	types := map[string]decl{}
	type method struct {
		fn   *ast.FuncDecl
		file *ast.File
	}
	methodsOf := map[string][]method{}
	names := slices.Sorted(maps.Keys(files))
	pos := func(p token.Pos) string {
		at := fset.Position(p)
		return fmt.Sprintf("%s:%d", filepath.Base(at.Filename), at.Line)
	}

	for _, name := range names {
		for _, d := range files[name].Decls {
			if d, ok := d.(*ast.GenDecl); ok {
				for _, s := range d.Specs {
					if ts, ok := s.(*ast.TypeSpec); ok {
						types[ts.Name.Name] = decl{ts, files[name]}
					}
				}
			}
		}
	}
	router := spellings(files, map[string]bool{"Router": true}, false)
	forbidden := spellings(files, routerForbidden, true)

	for _, name := range names {
		f := files[name]
		for _, d := range f.Decls {
			d, ok := d.(*ast.FuncDecl)
			if !ok || d.Recv == nil || len(d.Recv.List) != 1 {
				continue
			}
			base := recvBase(d.Recv.List[0].Type)
			methodsOf[base] = append(methodsOf[base], method{d, f})
			if router[base] {
				m.methods = append(m.methods, pos(d.Pos())+" "+d.Name.Name)
			}
		}
		// Router named anywhere but a receiver or its own declaration.
		count := func(n ast.Node) bool { return countRouterRef(n, router, &m, pos) }
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Type != nil {
					ast.Inspect(x.Type, count)
				}
				if x.Body != nil {
					ast.Inspect(x.Body, count)
				}
				return false
			}
			return count(n)
		})
	}

	routerDecl, ok := types["Router"]
	var st *ast.StructType
	if ok {
		st, ok = routerDecl.spec.Type.(*ast.StructType)
	}
	if !ok {
		return m
	}
	m.structFound = true

	// The facet closure: every session type a facet holds, reachable from
	// Router's fields other than ss through any type expression, interface
	// methods included. A reached type's own methods may not ask for the
	// table either, but what they take is not held, so the walk stops there.
	seen := map[string]bool{}
	var queue []string
	visit := func(at ast.Node, file *ast.File, where string, follow bool) {
		table := tableImportName(file)
		typeRefs(at, func(id *ast.Ident) {
			if forbidden[id.Name] {
				m.isolation = append(m.isolation, fmt.Sprintf("%s: %s names %s", pos(id.Pos()), where, id.Name))
				return
			}
			if _, declared := types[id.Name]; follow && declared && !seen[id.Name] {
				seen[id.Name] = true
				queue = append(queue, id.Name)
			}
		}, func(pkg, sel *ast.Ident) {
			if table != "" && pkg.Name == table {
				m.isolation = append(m.isolation, fmt.Sprintf("%s: %s names %s.%s", pos(pkg.Pos()), where, pkg.Name, sel.Name))
			}
		})
	}
	for _, fl := range st.Fields.List {
		if len(fl.Names) == 0 {
			m.fields = append(m.fields, recvBase(fl.Type))
		}
		skip := false
		for _, n := range fl.Names {
			m.fields = append(m.fields, n.Name)
			skip = skip || n.Name == "ss"
		}
		if !skip {
			visit(fl.Type, routerDecl.file, "Router field", true)
		}
	}
	// A struct standing in for Router holds its own fields next to the
	// embedded Router: they are Router's fields under another name.
	for _, name := range slices.Sorted(maps.Keys(router)) {
		d, declared := types[name]
		sst, isStruct := d.spec.Type.(*ast.StructType)
		if name == "Router" || !declared || !isStruct {
			continue
		}
		for _, fl := range sst.Fields.List {
			if len(fl.Names) == 0 && router[recvBase(fl.Type)] {
				continue
			}
			if len(fl.Names) == 0 {
				m.fields = append(m.fields, name+"."+recvBase(fl.Type))
			}
			for _, n := range fl.Names {
				m.fields = append(m.fields, name+"."+n.Name)
			}
			visit(fl.Type, d.file, name+" field", true)
		}
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		d := types[name]
		if d.spec.TypeParams != nil {
			visit(d.spec.TypeParams, d.file, name, true)
		}
		visit(d.spec.Type, d.file, name, true)
		for _, mt := range methodsOf[name] {
			visit(mt.fn.Type, mt.file, name+"."+mt.fn.Name.Name, false)
		}
	}
	m.closure = slices.Sorted(maps.Keys(seen))
	return m
}

// spellings is seed plus every type, at any scope, that stands in for a name
// already in the set, chains included. For Router that is an alias whose
// right-hand side mentions it (`type R = Router`, `type RP = *Router`,
// `type RR = R`), a defined type over it (`type X Router`, `type X *R`: the
// same fields, so its methods reach r.ss) and a struct embedding it
// (`struct{ *Router }`: its fields and methods are promoted). With table, a
// type standing in for a sessiontable type joins the same three ways. A
// struct that holds Router in a named field (`struct{ r *Router }`) is not a
// spelling: its declaration is one reference.
func spellings(files map[string]*ast.File, seed map[string]bool, table bool) map[string]bool {
	type spec struct {
		spec  *ast.TypeSpec
		table string
	}
	var specs []spec
	for _, f := range files {
		imp := tableImportName(f)
		ast.Inspect(f, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				specs = append(specs, spec{ts, imp})
			}
			return true
		})
	}
	set := maps.Clone(seed)
	for grew := true; grew; {
		grew = false
		for _, s := range specs {
			name := s.spec.Name.Name
			join := func() {
				if !set[name] {
					set[name], grew = true, true
				}
			}
			if s.spec.Assign.IsValid() {
				typeRefs(s.spec.Type, func(id *ast.Ident) {
					if set[id.Name] {
						join()
					}
				}, func(pkg, _ *ast.Ident) {
					if table && s.table != "" && pkg.Name == s.table {
						join()
					}
				})
				continue
			}
			// A defined type stands in for what its underlying type names;
			// a struct for what it embeds.
			under := []ast.Expr{s.spec.Type}
			if st, ok := s.spec.Type.(*ast.StructType); ok {
				under = nil
				for _, fl := range st.Fields.List {
					if len(fl.Names) == 0 {
						under = append(under, fl.Type)
					}
				}
			}
			for _, u := range under {
				switch b := baseType(u).(type) {
				case *ast.Ident:
					if set[b.Name] {
						join()
					}
				case *ast.SelectorExpr:
					if pkg, ok := b.X.(*ast.Ident); ok && table && s.table != "" && pkg.Name == s.table {
						join()
					}
				}
			}
		}
	}
	return set
}

// countRouterRef records an identifier that names Router under one of its
// spellings. Field, method and selected names, keyed literal fields and a
// type's own name are names, not references.
func countRouterRef(n ast.Node, router map[string]bool, m *routerMeasure, pos func(token.Pos) string) bool {
	count := func(n ast.Node) bool { return countRouterRef(n, router, m, pos) }
	switch x := n.(type) {
	case *ast.Field:
		if x.Type != nil {
			ast.Inspect(x.Type, count)
		}
		return false
	case *ast.TypeSpec:
		if x.TypeParams != nil {
			ast.Inspect(x.TypeParams, count)
		}
		ast.Inspect(x.Type, count)
		return false
	case *ast.SelectorExpr:
		ast.Inspect(x.X, count)
		return false
	case *ast.KeyValueExpr:
		if _, isName := x.Key.(*ast.Ident); !isName {
			ast.Inspect(x.Key, count)
		}
		ast.Inspect(x.Value, count)
		return false
	case *ast.Ident:
		switch {
		case x.Name == "Router":
			m.refs = append(m.refs, pos(x.Pos()))
		case router[x.Name]:
			m.refs = append(m.refs, pos(x.Pos())+" ("+x.Name+")")
		}
	}
	return true
}

// budgetProblems compares a measure with its limits; each problem starts
// with the rule it breaks.
func budgetProblems(m routerMeasure, l routerLimits) []string {
	var out []string
	add := func(rule, format string, args ...any) {
		out = append(out, rule+": "+fmt.Sprintf(format, args...))
	}
	strict := func(rule, what string, got []string, baseline int, constName string) {
		switch {
		case len(got) > baseline:
			add(rule, "%s grew: %d > baseline %d. Move the responsibility to a facet instead (#3023).\n  %s", what, len(got), baseline, strings.Join(got, "\n  "))
		case len(got) < baseline:
			add(rule, "%s dropped to %d (baseline %d): lower %s to %d.", what, len(got), baseline, constName, len(got))
		}
	}
	switch {
	case m.files < l.minFiles:
		add("blind", "parsed %d files, below the floor of %d", m.files, l.minFiles)
	case !m.structFound:
		add("blind", "no `type Router struct` found")
	case len(m.methods) < l.minMethods:
		add("blind", "found %d Router methods, below the floor of %d", len(m.methods), l.minMethods)
	case len(m.closure) < l.minClosure:
		add("blind", "facet closure holds %d types, below the floor of %d", len(m.closure), l.minClosure)
	}
	strict("fields", "Router fields", m.fields, l.fields, "routerFieldBaseline")
	strict("methods", "Router methods", m.methods, l.methods, "routerMethodBaseline")
	strict("refs", "references to Router outside receivers", m.refs, l.refs, "routerTypeRefBaseline")
	for _, v := range m.isolation {
		add("isolation", "%s: a facet reaches the session table without Router's transactions", v)
	}
	for _, name := range slices.Sorted(maps.Keys(m.lines)) {
		n := m.lines[name]
		base, exempt := l.exempt[name]
		switch {
		case exempt && n <= l.lineLimit:
			add("lines", "%s is %d lines, within the %d limit: delete its exemption constant", name, n, l.lineLimit)
		case exempt && n > base:
			add("lines", "%s grew to %d lines, above its exemption of %d: split it", name, n, base)
		case exempt && base-n > routerLinesSlackBaseline:
			add("lines", "%s is %d lines, %d below its exemption of %d (slack %d): re-sample the constant to %d", name, n, base-n, base, routerLinesSlackBaseline, n)
		case !exempt && n > l.lineLimit:
			add("lines", "%s is %d lines, above the %d limit: split it", name, n, l.lineLimit)
		}
	}
	switch {
	case len(l.exempt) > l.exemptions:
		add("exemptions", "%d files are exempt from the line limit, above the baseline of %d: split the file instead (#3023)", len(l.exempt), l.exemptions)
	case len(l.exempt) < l.exemptions:
		add("exemptions", "%d files are exempt from the line limit (baseline %d): lower routerLineExemptionsBaseline to %d", len(l.exempt), l.exemptions, len(l.exempt))
	}
	for name := range l.exempt {
		if _, ok := m.lines[name]; !ok {
			add("lines", "exemption for %s, which no longer exists: delete its constant", name)
		}
	}
	return out
}

func measureRealRouter(t *testing.T) routerMeasure {
	t.Helper()
	srcs := readRouterSources(t)
	lines := map[string]int{}
	for name, src := range srcs {
		lines[name] = lineCount([]byte(src))
	}
	fset, files := parseRouterSources(t, srcs)
	return measureRouter(fset, files, lines)
}

func TestRouterBudget(t *testing.T) {
	t.Parallel()
	m := measureRealRouter(t)
	l := realRouterLimits()
	t.Logf("files %d, fields %d, methods %d, refs %d (%s); router_core.go %d lines, router_lifecycle.go %d lines; S12 target fields ≤%d, methods ≤%d",
		m.files, len(m.fields), len(m.methods), len(m.refs), strings.Join(m.refs, ", "),
		m.lines["router_core.go"], m.lines["router_lifecycle.go"], routerFieldTargetBaseline, routerMethodTargetBaseline)
	for _, p := range budgetProblems(m, l) {
		if !strings.HasPrefix(p, "isolation: ") {
			t.Error(p)
		}
	}
}

func TestRouterFacetIsolation(t *testing.T) {
	t.Parallel()
	m := measureRealRouter(t)
	t.Logf("facet closure, %d types: %s; %d violations", len(m.closure), strings.Join(m.closure, ", "), len(m.isolation))
	if !m.structFound || len(m.closure) < realRouterLimits().minClosure {
		t.Fatalf("facet closure holds %d types (Router found: %v), below the floor of %d: the walk has gone blind",
			len(m.closure), m.structFound, realRouterLimits().minClosure)
	}
	for _, v := range m.isolation {
		t.Errorf("isolation: %s: a facet reaches the session table without Router's transactions", v)
	}
}

// routerFixture is a package that meets its limits exactly: every rule has
// something to count, and the closure runs through a pointer, a map of
// slices of channels, an interface method's parameters and results, a
// generic argument and a function field.
var routerFixture = map[string]string{
	"router.go": `package session
import st "github.com/naozhi/naozhi/internal/session/sessiontable"
type routerState struct{}
type routerStateView struct{}
type (
	sessTx   = st.Tx[int, routerState, routerStateView]
	sessView = st.View[int, routerState, routerStateView]
)
type Router struct {
	ss       *st.Table[int, routerState, routerStateView]
	a        facetA
	hook     func(facetG) facetH
	ttl, ttl2 int
}
func NewRouter() *Router { return &Router{a: facetA{}} }
func (r *Router) One(tx sessTx) {}
func (r Router) Two(v sessView) {}
`,
	"facets.go": `package session
type facetA struct{ b *facetB }
type facetB struct{ m map[string][]chan facetC }
type facetC interface{ Do(x facetD) (facetE, error) }
type facetD struct{ b box[facetF] }
type box[T any] struct{ v T }
type facetE struct{}
type facetF struct{}
type facetG struct{}
type facetH struct{}
func (a *facetA) peek(n int) bool { return n > 0 }
func (a *facetA) with(s *session) {}
type session struct{ r *Router }
`,
}

var routerFixtureLimits = routerLimits{fields: 5, methods: 2, refs: 3, lineLimit: 900, exemptions: 1, exempt: map[string]int{"big.go": 1000}}

var routerFixtureLines = map[string]int{"router.go": 20, "facets.go": 12, "big.go": 990}

func measureFixture(t *testing.T, override map[string]string, lines map[string]int) routerMeasure {
	t.Helper()
	srcs := map[string]string{}
	for k, v := range routerFixture {
		srcs[k] = v
	}
	for k, v := range override {
		srcs[k] = v
	}
	fset, files := parseRouterSources(t, srcs)
	if lines == nil {
		lines = routerFixtureLines
	}
	return measureRouter(fset, files, lines)
}

func TestRouterBudget_FixtureIsClean(t *testing.T) {
	m := measureFixture(t, nil, nil)
	if got := budgetProblems(m, routerFixtureLimits); len(got) != 0 {
		t.Fatalf("the fixture meets its limits but was flagged: %q", got)
	}
	want := []string{"box", "facetA", "facetB", "facetC", "facetD", "facetE", "facetF", "facetG", "facetH"}
	if !slices.Equal(m.closure, want) {
		t.Errorf("closure = %v, want %v (ss and the session table it names stay outside)", m.closure, want)
	}
}

// Each rule fires on the shape it forbids, so a gate that silently stopped
// matching fails here rather than passes everything.
func TestRouterBudget_CatchesEachRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override map[string]string
		lines    map[string]int
		limits   func(*routerLimits)
		rules    []string
	}{
		{"field added", map[string]string{"router.go": strings.Replace(routerFixture["router.go"], "ttl, ttl2 int", "ttl, ttl2 int\n\tsync.Mutex", 1)}, nil, nil, []string{"fields"}},
		{"field removed", map[string]string{"router.go": strings.Replace(routerFixture["router.go"], "ttl, ttl2 int", "ttl int", 1)}, nil, nil, []string{"fields"}},
		{"pointer receiver method", map[string]string{"x.go": "package session\nfunc (r *Router) Three() {}\n"}, nil, nil, []string{"methods"}},
		{"value receiver method", map[string]string{"x.go": "package session\nfunc (r Router) Three() {}\n"}, nil, nil, []string{"methods"}},
		{"method removed", map[string]string{"router.go": strings.Replace(routerFixture["router.go"], "func (r Router) Two(v sessView) {}", "", 1)}, nil, nil, []string{"methods"}},
		{"alias with methods", map[string]string{"x.go": "package session\ntype R = Router\nfunc (r *R) X() {}\n"}, nil, nil, []string{"methods", "refs"}},
		{"alias chain with methods", map[string]string{"x.go": "package session\ntype RR = R\ntype R = Router\nfunc (r RR) X() {}\n"}, nil, nil, []string{"methods", "refs"}},
		// The alias declaration costs one reference; dropping a *Router
		// elsewhere pays for it, so only the method count can see the method.
		{"alias paid for by a dropped reference", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type routerAlias = Router\nfunc (r *routerAlias) Hidden() {}", 1)}, nil, nil, []string{"methods"}},
		// Under the alias every use is a reference, so swapping a *Router
		// for *routerAlias no longer hides the package func taking it.
		{"alias replaces an existing *Router", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }",
			"type session struct{ r *routerAlias }\ntype routerAlias = Router\nfunc (r *routerAlias) HiddenOne() {}\nfunc hiddenFree(r *routerAlias) {}", 1)}, nil, nil, []string{"methods", "refs"}},
		{"pointer alias stands in for *Router", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type session struct{ r RP }\ntype RP = *Router", 1)}, nil, nil, []string{"refs"}},
		{"facet method takes a Router alias", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"peek(n int)", "peek(r *R)", 1) + "type R = Router\n"}, nil, nil, []string{"isolation", "refs"}},
		{"facet method takes an alias of sessTx", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"peek(n int)", "peek(tx myTx)", 1) + "type myTx = sessTx\n"}, nil, nil, []string{"isolation"}},
		{"facet method takes an alias of the table", map[string]string{"facets.go": strings.NewReplacer(
			"package session\n", "package session\nimport st \""+sessiontableImport+"\"\n",
			"peek(n int)", "peek(t *myTable)").Replace(routerFixture["facets.go"]) + "type myTable = st.Table[int, int, int]\n"}, nil, nil, []string{"isolation"}},
		// A struct embedding Router has its fields and methods: it is Router.
		{"embedding wrapper with methods", map[string]string{"x.go": "package session\ntype routerExt struct{ *Router }\nfunc (r *routerExt) sneaky() bool { return r.ss == nil }\n"}, nil, nil, []string{"methods", "refs"}},
		{"embedding wrapper paid for by a dropped reference", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type routerExt struct{ *Router }\nfunc (r *routerExt) Hidden() bool { return r.ss == nil }", 1)}, nil, nil, []string{"methods"}},
		{"embedding wrapper replaces an existing *Router", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }",
			"type session struct{ r *routerExt }\ntype routerExt struct{ *Router }\nfunc (r *routerExt) HiddenOne() {}\nfunc newSession(r *Router) session { return session{r: &routerExt{r}} }", 1)}, nil, nil, []string{"methods", "refs"}},
		{"embedding chain with methods", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type outer struct{ inner }\ntype inner struct{ *Router }\nfunc (o outer) Hidden() {}", 1)}, nil, nil, []string{"methods", "refs"}},
		{"embedding a Router alias", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type routerExt struct{ R }\ntype R = Router\nfunc (r routerExt) Hidden() {}", 1)}, nil, nil, []string{"methods", "refs"}},
		{"embedding wrapper's own field", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type routerExt struct{\n\t*Router\n\textra map[string]int\n}", 1)}, nil, nil, []string{"fields"}},
		{"embedding wrapper's field reaches the table", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type routerExt struct{\n\t*Router\n\tpeek func(sessView)\n}", 1)}, nil, nil, []string{"fields", "isolation"}},
		// A defined type over Router has its fields, so its methods reach r.ss.
		{"defined type over Router with methods", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type routerX Router\nfunc (x *routerX) Hidden() bool { return x.ss == nil }", 1)}, nil, nil, []string{"methods"}},
		{"defined type over Router replaces an existing *Router", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type session struct{ r *routerX }\ntype routerX Router", 1)}, nil, nil, []string{"refs"}},
		{"facet method takes a struct embedding sessTx", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"peek(n int)", "peek(t txExt)", 1) + "type txExt struct{ sessTx }\n"}, nil, nil, []string{"isolation"}},
		{"facet method takes a struct embedding the table", map[string]string{"facets.go": strings.NewReplacer(
			"package session\n", "package session\nimport st \""+sessiontableImport+"\"\n",
			"peek(n int)", "peek(t tableExt)").Replace(routerFixture["facets.go"]) + "type tableExt struct{ *st.Table[int, int, int] }\n"}, nil, nil, []string{"isolation"}},
		{"facet method takes a defined type over routerState", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"peek(n int)", "peek(s myState)", 1) + "type myState routerState\n"}, nil, nil, []string{"isolation"}},
		{"wrapper struct", map[string]string{"x.go": "package session\ntype routerOps struct{ r *Router }\n"}, nil, nil, []string{"refs"}},
		// Accepted limit of a count: a struct holding Router in a named field
		// costs one reference, and its methods are not Router methods. Paid for
		// by a dropped reference, it passes, the same as ScratchPool's methods
		// reaching p.router today. Pinned so a change to it is deliberate.
		{"named-field holder paid for by a dropped reference", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type session struct{ r *Router }", "type routerOps struct{ r *Router }\nfunc (o *routerOps) Hidden() bool { return o.r.ss == nil }", 1)}, nil, nil, nil},
		{"package func param", map[string]string{"x.go": "package session\nfunc resetFoo(r *Router) {}\n"}, nil, nil, []string{"refs"}},
		{"conversion", map[string]string{"x.go": "package session\nvar _ = (*Router)(nil)\n"}, nil, nil, []string{"refs"}},
		{"the table imported but not named by a facet", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"package session\n", "package session\nimport st \""+sessiontableImport+"\"\n", 1)}, nil, nil, nil},
		{"generic session table in a facet", map[string]string{"facets.go": strings.NewReplacer(
			"package session\n", "package session\nimport st \""+sessiontableImport+"\"\n",
			"type facetE struct{}", "type facetE struct{ t *st.Table[int, int, int] }").Replace(routerFixture["facets.go"])}, nil, nil, []string{"isolation"}},
		{"func(sessView) field", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type facetF struct{}", "type facetF struct{ lookup func(sessView) string }", 1)}, nil, nil, []string{"isolation"}},
		{"interface method takes sessTx", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"Do(x facetD) (facetE, error)", "Do(x facetD) (facetE, error); Peek(tx sessTx)", 1)}, nil, nil, []string{"isolation"}},
		{"facet method takes routerStateView", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"peek(n int)", "peek(v routerStateView)", 1)}, nil, nil, []string{"isolation"}},
		{"two hops to *Router", map[string]string{"facets.go": strings.Replace(routerFixture["facets.go"],
			"type facetH struct{}", "type facetH struct{ back *facetI }\ntype facetI struct{ r *Router }", 1)}, nil, nil, []string{"isolation", "refs"}},
		{"file over the limit", nil, map[string]int{"router.go": 901, "facets.go": 12, "big.go": 990}, nil, []string{"lines"}},
		{"exempt file grew", nil, map[string]int{"router.go": 20, "facets.go": 12, "big.go": 1001}, nil, []string{"lines"}},
		{"exempt file shrank past the slack", nil, map[string]int{"router.go": 20, "facets.go": 12, "big.go": 969}, nil, []string{"lines"}},
		{"exempt file back under the limit", nil, map[string]int{"router.go": 20, "facets.go": 12, "big.go": 900}, nil, []string{"lines"}},
		{"exempt file deleted", nil, map[string]int{"router.go": 20, "facets.go": 12}, nil, []string{"lines"}},
		{"exemption added", nil, map[string]int{"router.go": 20, "facets.go": 12, "big.go": 990, "huge.go": 950},
			func(l *routerLimits) { l.exempt = map[string]int{"big.go": 1000, "huge.go": 950} }, []string{"exemptions"}},
		{"exemption dropped", nil, map[string]int{"router.go": 20, "facets.go": 12, "big.go": 990},
			func(l *routerLimits) { l.exemptions = 2 }, []string{"exemptions"}},
		{"no Router struct", map[string]string{"router.go": "package session\ntype Router interface{}\n"}, nil,
			func(l *routerLimits) { l.fields, l.methods, l.refs = 0, 0, 1 }, []string{"blind"}},
		{"too few files", nil, nil, func(l *routerLimits) { l.minFiles = 3 }, []string{"blind"}},
		{"too few methods", nil, nil, func(l *routerLimits) { l.minMethods = 3 }, []string{"blind"}},
		{"closure too small", nil, nil, func(l *routerLimits) { l.minClosure = 10 }, []string{"blind"}},
	} {
		l := routerFixtureLimits
		if tc.limits != nil {
			tc.limits(&l)
		}
		got := budgetProblems(measureFixture(t, tc.override, tc.lines), l)
		var rules []string
		for _, p := range got {
			rule, _, _ := strings.Cut(p, ": ")
			if !slices.Contains(rules, rule) {
				rules = append(rules, rule)
			}
		}
		slices.Sort(rules)
		if !slices.Equal(rules, tc.rules) {
			t.Errorf("%s: rules fired = %v, want %v\n  %s", tc.name, rules, tc.rules, strings.Join(got, "\n  "))
		}
	}
}
