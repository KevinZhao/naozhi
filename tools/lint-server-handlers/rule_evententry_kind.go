// Rule evententry_kind (#2897 S13b): an EventEntry kind is spelled with a
// clievent.Kind* constant, never a string literal. EventEntry.Type stays a
// plain string (a named Kind type cascaded compile errors through every
// downstream package), so the vocabulary is closed by this typed check over
// every non-test package of the module except clievent itself.
//
// Kind positions are:
//
//  1. the Type value of an EventEntry composite literal;
//  2. the right side of an assignment to the Type field of an EventEntry;
//  3. the other side of == / != against an EventEntry's Type, and the cases of
//     a switch on it;
//  4. kind parameters: a parameter that appears in a kind position of its own
//     function (history.NewDerivedEntry's entryType) makes the matching
//     argument of every static call a kind position. The reverse holds too: a
//     parameter of a module function that every static call feeds a kind (an
//     entry's Type, a kind variable or parameter, a Kind* constant) or a string
//     literal, at least one a kind, and that is never taken as a value, is a
//     kind parameter (ring's entryAffectsAgentState), so handing a kind to a
//     helper does not hide the helper's comparisons;
//  5. kind variables: a local that appears in a kind position, or is set from
//     an EventEntry's Type, makes its every assignment and comparison one, the
//     elements (or map keys) of a ranged composite literal included.
//
// A kind position accepts a clievent Kind* constant whose value
// clievent.IsKnownKind reports registered (this tool links the real clievent,
// so deleting a kindTable row rejects every use of its constant), a copy of
// another EventEntry's Type, or a traced kind parameter / variable. Anything
// else fails — every string literal, a registered "text" included, another
// package's constant, a field read, a call result.
//
// Not covered: a kind reaching Type through a struct field, a function result,
// a closure parameter or reflection; a helper parameter some call feeds a
// non-kind (or reached through a value, an interface or a variadic); a range
// over a non-literal; set membership (map index, slices.Contains); Type read
// through strings.* helpers; and structs mirroring EventEntry
// (sysession.SystemEventEntry, persist's gapEntryJSON, pinned by its own test).
// Sentinels pin files and packages that must keep producing kind positions.
package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// kindSentinels are module-relative files or packages that must each hold at
// least one kind position, so a refactor that hides them from the rule fails.
var kindSentinels = []string{
	"internal/cli/process_event_format.go",
	"internal/subagent/transcript.go",
	"internal/eventlog/ring",
}

// kindSite is one kind position and its verdict.
type kindSite struct {
	Pkg, File string
	Line      int
	Where     string // literal | assign | compare | case | arg | var | range
	Form      string
	Verdict   string // ok | violation
	Why       string
}

type kindParamKey struct {
	Func  string // module-relative FullName
	Index int
}

type kindWalker struct {
	prog      *typedProgram
	pkg       *typedPkg
	params    map[kindParamKey]bool
	vars      map[*types.Var]bool
	positions []kindPos // of the latest pass
	funcs     []*kindFunc
	// Call-site evidence for the reverse of rule 4, of the latest pass.
	calls   map[kindParamKey]*kindArgs
	called  map[*ast.Ident]bool
	asValue map[string]bool
}

// kindArgs: some static call passes a kind to the parameter, or a non-kind
// other than a string literal.
type kindArgs struct{ kind, other bool }

type kindPos struct {
	e     ast.Expr
	where string
	pkg   *typedPkg
	fn    *kindFunc
}

type kindFunc struct {
	name   string // FullName, "" for a function literal
	params map[*types.Var]int
	body   *ast.BlockStmt
}

// scanEventEntryKind returns every kind position with its verdict, and the
// violations among them plus missing sentinels.
func scanEventEntryKind(prog *typedProgram, sentinels []string) ([]kindSite, []Violation) {
	w := &kindWalker{prog: prog, params: map[kindParamKey]bool{}, vars: map[*types.Var]bool{}}
	// Kind parameters and variables are found by a fixed point: each pass can
	// promote a parameter or local, which adds positions for the next one.
	for {
		before := len(w.params) + len(w.vars)
		w.collect()
		for _, p := range w.positions {
			w.absorb(p)
		}
		w.promoteCallees()
		if len(w.params)+len(w.vars) == before {
			break
		}
	}
	var sites []kindSite
	for _, p := range w.positions {
		sites = append(sites, w.classify(p))
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	var vs []Violation
	for _, s := range sites {
		if s.Verdict == "violation" {
			vs = append(vs, Violation{Rule: "evententry_kind", File: s.File, Line: s.Line, Message: fmt.Sprintf(
				"%s in an EventEntry kind position (%s): %s; spell the kind with a clievent.Kind* constant (registered in clievent kindTable) or copy another entry's Type", s.Form, s.Where, s.Why)})
		}
	}
	for _, want := range sentinels {
		hit := false
		for _, s := range sites {
			if s.File == want || s.Pkg == want {
				hit = true
				break
			}
		}
		if !hit {
			vs = append(vs, Violation{Rule: "evententry_kind", File: want, Message: fmt.Sprintf(
				"sentinel %s holds no EventEntry kind position: the rule has gone blind there, or the producer moved (update the sentinel)", want)})
		}
	}
	return sites, vs
}

// collect walks every package and records the kind positions known so far.
func (w *kindWalker) collect() {
	w.positions = w.positions[:0]
	w.calls, w.called, w.asValue = map[kindParamKey]*kindArgs{}, map[*ast.Ident]bool{}, map[string]bool{}
	for _, p := range w.prog.Pkgs {
		if p.Rel == entryPkg {
			continue
		}
		w.pkg = p
		for _, f := range p.Files {
			ast.Inspect(f, w.visit)
		}
	}
}

func (w *kindWalker) info() *types.Info { return w.pkg.Info }

func (w *kindWalker) cur() *kindFunc {
	if len(w.funcs) == 0 {
		return nil
	}
	return w.funcs[len(w.funcs)-1]
}

func (w *kindWalker) add(e ast.Expr, where string) {
	w.positions = append(w.positions, kindPos{e: e, where: where, pkg: w.pkg, fn: w.cur()})
}

func (w *kindWalker) enter(name string, ftype *ast.FuncType, body *ast.BlockStmt) {
	f := &kindFunc{name: name, params: map[*types.Var]int{}, body: body}
	i := 0
	for _, fld := range ftype.Params.List {
		if len(fld.Names) == 0 {
			i++
		}
		for _, id := range fld.Names {
			if v, ok := w.info().Defs[id].(*types.Var); ok {
				f.params[v] = i
			}
			i++
		}
	}
	w.funcs = append(w.funcs, f)
}

func (w *kindWalker) visit(n ast.Node) bool {
	info := w.info()
	switch x := n.(type) {
	case *ast.FuncDecl:
		if x.Body == nil {
			return false
		}
		name := ""
		if fn, ok := info.Defs[x.Name].(*types.Func); ok {
			name = relToModule(w.prog.Module, fn.FullName())
		}
		w.enter(name, x.Type, x.Body)
		ast.Inspect(x.Body, w.visit)
		w.funcs = w.funcs[:len(w.funcs)-1]
		return false
	case *ast.FuncLit:
		w.enter("", x.Type, x.Body)
		ast.Inspect(x.Body, w.visit)
		w.funcs = w.funcs[:len(w.funcs)-1]
		return false
	case *ast.CompositeLit:
		if w.isEntry(info.TypeOf(x)) {
			for _, el := range x.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Type" {
						w.add(kv.Value, "literal")
					}
				}
			}
		}
	case *ast.AssignStmt:
		if len(x.Lhs) != len(x.Rhs) {
			break
		}
		for i, l := range x.Lhs {
			if x.Tok != token.ASSIGN && x.Tok != token.DEFINE {
				break
			}
			w.promoteCopy(l, x.Rhs[i])
			switch {
			case w.isEntryType(l):
				w.add(x.Rhs[i], "assign")
			case w.isKindVar(l):
				w.add(x.Rhs[i], "var")
			}
		}
	case *ast.ValueSpec:
		for i, id := range x.Names {
			if i >= len(x.Values) {
				break
			}
			w.promoteCopy(id, x.Values[i])
			if w.isKindVar(id) {
				w.add(x.Values[i], "var")
			}
		}
	case *ast.BinaryExpr:
		if x.Op == token.EQL || x.Op == token.NEQ {
			if w.isAnchor(x.X) {
				w.add(x.Y, "compare")
			}
			if w.isAnchor(x.Y) {
				w.add(x.X, "compare")
			}
		}
	case *ast.SwitchStmt:
		if x.Tag != nil && w.isAnchor(x.Tag) {
			for _, st := range x.Body.List {
				for _, e := range st.(*ast.CaseClause).List {
					w.add(e, "case")
				}
			}
		}
	case *ast.RangeStmt:
		if x.Value != nil && w.isKindVar(x.Value) {
			w.addRangeLit(x.X, false)
		}
		if x.Key != nil && w.isKindVar(x.Key) {
			w.addRangeLit(x.X, true)
		}
	case *ast.Ident:
		if fn, ok := info.Uses[x].(*types.Func); ok && !w.called[x] {
			w.asValue[relToModule(w.prog.Module, fn.Origin().FullName())] = true
		}
	case *ast.CallExpr:
		w.noteCall(x)
		if name := w.staticCallee(x); name != "" {
			for i, a := range x.Args {
				if w.params[kindParamKey{name, i}] {
					w.add(a, "arg")
				}
			}
		}
	}
	return true
}

// addRangeLit makes the elements (key: the map keys) of a ranged composite
// literal kind positions.
func (w *kindWalker) addRangeLit(x ast.Expr, key bool) {
	lit, ok := ast.Unparen(x).(*ast.CompositeLit)
	if !ok {
		return
	}
	for _, el := range lit.Elts {
		kv, isKV := el.(*ast.KeyValueExpr)
		switch {
		case isKV && key:
			w.add(kv.Key, "range")
		case isKV:
			w.add(kv.Value, "range")
		case !key:
			w.add(el, "range")
		}
	}
}

// noteCall records what a static call to a module function passes to each
// parameter, for promoteCallees.
func (w *kindWalker) noteCall(c *ast.CallExpr) {
	fn, id := w.callee(c)
	if fn == nil {
		return
	}
	w.called[id] = true
	if fn.Pkg() == nil || relToModule(w.prog.Module, fn.Pkg().Path()) == fn.Pkg().Path() {
		return
	}
	sig := fn.Type().(*types.Signature)
	name := relToModule(w.prog.Module, fn.Origin().FullName())
	for i, a := range c.Args {
		k := kindParamKey{name, i}
		st := w.calls[k]
		if st == nil {
			st = &kindArgs{}
			w.calls[k] = st
		}
		lit, isLit := ast.Unparen(a).(*ast.BasicLit)
		switch {
		case sig.Variadic() && i >= sig.Params().Len()-1:
			st.other = true
		case w.isAnchor(a) || w.isKindConst(a):
			st.kind = true
		case isLit && lit.Kind == token.STRING:
		default:
			st.other = true
		}
	}
}

// promoteCallees is the reverse of rule 4: a parameter every static call feeds
// a kind or a string literal, at least one a kind, becomes a kind parameter.
func (w *kindWalker) promoteCallees() {
	for k, a := range w.calls {
		if a.kind && !a.other && !w.asValue[k.Func] {
			w.params[k] = true
		}
	}
}

// isKindConst: e names a clievent Kind* constant, registered or not (classify
// judges registration).
func (w *kindWalker) isKindConst(e ast.Expr) bool {
	var id *ast.Ident
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	}
	if id == nil {
		return false
	}
	o, ok := w.info().Uses[id].(*types.Const)
	return ok && o.Pkg() != nil && relToModule(w.prog.Module, o.Pkg().Path()) == entryPkg && strings.HasPrefix(o.Name(), "Kind")
}

// absorb promotes the parameter or local a position names, and a local set
// from an EventEntry's Type.
func (w *kindWalker) absorb(p kindPos) {
	id, ok := ast.Unparen(p.e).(*ast.Ident)
	if !ok || p.fn == nil {
		return
	}
	v, ok := p.pkg.Info.Uses[id].(*types.Var)
	if !ok {
		return
	}
	if i, isParam := p.fn.params[v]; isParam {
		if p.fn.name != "" {
			w.params[kindParamKey{p.fn.name, i}] = true
		}
		return
	}
	if v.Pos() >= p.fn.body.Pos() && v.Pos() < p.fn.body.End() {
		w.vars[v] = true
	}
}

// promoteCopy makes a local set from an EventEntry's Type a kind variable.
func (w *kindWalker) promoteCopy(lhs, rhs ast.Expr) {
	id, ok := ast.Unparen(lhs).(*ast.Ident)
	f := w.cur()
	if !ok || f == nil || !w.isEntryType(rhs) {
		return
	}
	v, ok := w.info().Defs[id].(*types.Var)
	if !ok {
		v, ok = w.info().Uses[id].(*types.Var)
	}
	if ok && v.Pos() >= f.body.Pos() && v.Pos() < f.body.End() {
		w.vars[v] = true
	}
}

func (w *kindWalker) classify(p kindPos) kindSite {
	info := p.pkg.Info
	pos := w.prog.Fset.Position(p.e.Pos())
	s := kindSite{Pkg: p.pkg.Rel, File: w.relFile(pos.Filename), Line: pos.Line, Where: p.where, Form: "`" + types.ExprString(p.e) + "`", Verdict: "violation"}
	e := ast.Unparen(p.e)
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s.Why = "a string literal, registered or not"
		return s
	}
	var id *ast.Ident
	switch x := e.(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		if w.isEntryTypeIn(info, x) {
			s.Verdict = "ok"
			return s
		}
		id = x.Sel
	}
	if id == nil {
		s.Why = "not a constant or an entry's Type"
		return s
	}
	switch o := info.Uses[id].(type) {
	case *types.Const:
		if o.Pkg() == nil || relToModule(w.prog.Module, o.Pkg().Path()) != entryPkg || !strings.HasPrefix(o.Name(), "Kind") {
			s.Why = "a constant outside clievent's Kind* set"
			return s
		}
		if o.Val().Kind() != constant.String || !clievent.IsKnownKind(constant.StringVal(o.Val())) {
			s.Why = fmt.Sprintf("%s is not registered in clievent kindTable", o.Name())
			return s
		}
		s.Verdict = "ok"
	case *types.Var:
		if w.vars[o] {
			s.Verdict = "ok"
			return s
		}
		if p.fn != nil && p.fn.name != "" {
			if i, ok := p.fn.params[o]; ok && w.params[kindParamKey{p.fn.name, i}] {
				s.Verdict = "ok"
				return s
			}
		}
		s.Why = "a variable the rule cannot trace (a field, a global, or a closure parameter)"
	default:
		s.Why = "not a constant or an entry's Type"
	}
	return s
}

// isAnchor: an EventEntry's Type, a kind variable or a kind parameter, whose
// comparison partners are kind positions.
func (w *kindWalker) isAnchor(e ast.Expr) bool {
	if w.isEntryType(e) || w.isKindVar(e) {
		return true
	}
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return false
	}
	if f := w.cur(); f != nil && f.name != "" {
		if v, ok := w.info().Uses[id].(*types.Var); ok {
			if i, ok := f.params[v]; ok {
				return w.params[kindParamKey{f.name, i}]
			}
		}
	}
	return false
}

// isKindVar reports a kind variable, promoting a local defined from an
// EventEntry's Type on the way (rule 5).
func (w *kindWalker) isKindVar(e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return false
	}
	v, ok := w.info().Uses[id].(*types.Var)
	if !ok {
		v, ok = w.info().Defs[id].(*types.Var)
	}
	return ok && w.vars[v]
}

func (w *kindWalker) isEntryType(e ast.Expr) bool {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	return ok && w.isEntryTypeIn(w.info(), sel)
}

// isEntryTypeIn: sel selects the Type field of an EventEntry, embedded or not.
func (w *kindWalker) isEntryTypeIn(info *types.Info, sel *ast.SelectorExpr) bool {
	s := info.Selections[sel]
	if s == nil || s.Kind() != types.FieldVal || s.Obj().Name() != "Type" {
		return false
	}
	t := s.Recv()
	idx := s.Index()
	for _, i := range idx[:len(idx)-1] {
		st, ok := deref(t).Underlying().(*types.Struct)
		if !ok {
			return false
		}
		t = st.Field(i).Type()
	}
	return w.isEntry(t)
}

func (w *kindWalker) isEntry(t types.Type) bool {
	if t == nil {
		return false
	}
	n, ok := types.Unalias(deref(t)).(*types.Named)
	return ok && n.Obj().Name() == entryType && n.Obj().Pkg() != nil &&
		relToModule(w.prog.Module, n.Obj().Pkg().Path()) == entryPkg
}

func (w *kindWalker) staticCallee(c *ast.CallExpr) string {
	if fn, _ := w.callee(c); fn != nil {
		return relToModule(w.prog.Module, fn.Origin().FullName())
	}
	return ""
}

// callee returns the function a call names and the identifier naming it.
func (w *kindWalker) callee(c *ast.CallExpr) (*types.Func, *ast.Ident) {
	var id *ast.Ident
	switch f := ast.Unparen(c.Fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	}
	if id == nil {
		return nil, nil
	}
	if fn, ok := w.info().Uses[id].(*types.Func); ok {
		return fn, id
	}
	return nil, nil
}

func (w *kindWalker) relFile(name string) string {
	return (&egressWalker{prog: w.prog}).relFile(name)
}

func deref(t types.Type) types.Type {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}
