// Rule wire_egress (#2897 S13b): a clievent.EventEntry leaves the process only
// as its wire view. clievent.ForWire clears the four linkage fields the local
// SubagentLinker reads and redacts credential shapes; an egress that forgets
// it ships jsonl_path and raw tokens to a browser or a peer node (#2951).
//
// Typed, over every non-test package of the module (typed_load.go). The check
// point is a conversion to an interface: wherever a value whose JSON-reachable
// part contains an EventEntry (through pointers, slice / array / map elements,
// or exported fields not tagged json:"-") is converted to an interface type —
// a call argument (including interface methods and builtins), a return, an
// assignment, a typed var, a composite-literal element / field / map value, a
// channel send, or an explicit any(x) — the value must be a wire view:
//
//  1. a direct call to one of wireEgress.Projectors (ForWire, ForWireOne,
//     wsproto.New*; TestEventFrames_CarryTheWireView pins the latter);
//  2. nil, an empty literal, or a composite literal of a carrier struct whose
//     every EventEntry-reaching field is itself a wire view (a non-empty
//     EventEntry literal never is);
//  3. a local variable whose declaration, every assignment, every range
//     source and every write through it to an EventEntry-reaching path is a
//     wire view, which is never address-taken (&v, a pointer-receiver call)
//     and, when it is a pointer / slice / map, never copied into another
//     variable.
//
// Anything else — a parameter, a field read, an index, another function's
// result — fails, and the fix is to project at the egress; there are no
// file:line waivers. Exemptions are by callee FullName with a reason, and an
// exemption nothing hits is itself a violation. Generic instantiations whose
// type arguments reach EventEntry are allowed only for the listed functions,
// since a type parameter hides the value from this check. Sentinels pin, per
// package, an egress callee that must keep producing sites, so a refactor that
// hides a package's egress from the rule fails instead of passing silently.
//
// Not covered: raw-byte paths (cron run events are json.RawMessage NDJSON with
// their own redactRawJSON), values smuggled through unsafe or reflect, and a
// reference-typed local handed to a function that mutates it.
package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// entryPkg / entryType name the guarded type, module-relative.
const (
	entryPkg  = "internal/cli/clievent"
	entryType = "EventEntry"
)

type egressExemption struct {
	Callee string // module-relative types.Func.FullName, or a pseudo-callee below
	File   string // when set, only sites in this module-relative file
	Why    string
}

type egressSentinel struct{ Pkg, Callee string }

type wireEgressRules struct {
	Projectors []string
	Exemptions []egressExemption
	Generics   []string // generic functions allowed to be instantiated with an EventEntry type
	Sentinels  []egressSentinel
}

// poolNewCallee is the pseudo-callee of a return inside a sync.Pool New func.
const poolNewCallee = "sync.Pool.New"

var wireEgress = wireEgressRules{
	Projectors: []string{
		"internal/cli/clievent.ForWire",
		"internal/cli/clievent.ForWireOne",
		"internal/wsproto.NewHistory",
		"internal/wsproto.NewEvent",
		"internal/wsproto.NewAgentEvent",
	},
	Exemptions: []egressExemption{
		{Callee: "encoding/json.Unmarshal", Why: "inbound decode: the value is a destination"},
		{Callee: "(*encoding/json.Decoder).Decode", Why: "inbound decode: the value is a destination"},
		{Callee: "(*github.com/gorilla/websocket.Conn).ReadJSON", Why: "inbound decode: the value is a destination"},
		{Callee: "sort.SliceStable", Why: "sorts in place, nothing is encoded"},
		{Callee: "sort.SliceIsSorted", Why: "reads in place, nothing is encoded"},
		{Callee: "(*sync.Pool).Put", Why: "buffer reuse, nothing is encoded"},
		{Callee: poolNewCallee, Why: "buffer reuse, nothing is encoded"},
		{Callee: "(*encoding/json.Encoder).Encode", File: "internal/session/eventlog_bridge.go", Why: "persistence must keep the linkage fields the wire view clears"},
	},
	Generics: []string{
		"slices.Reverse", "slices.IsSortedFunc", "slices.SortStableFunc",
		"slices.BinarySearchFunc", "slices.Clone",
		// A holder: Store / Load encode nothing, and what Load returns is
		// checked wherever it egresses.
		"sync/atomic.Pointer",
	},
	Sentinels: []egressSentinel{
		{"internal/server", "(*internal/server.wsClient).SendJSON"},
		{"internal/node", "(internal/node.EventSink).SendJSON"},
		{"internal/upstream", "writeJSON"},
		{"internal/upstream", "internal/upstream.marshalResult"},
		{"internal/dashboard/session", "internal/dashboard/httputil.WriteJSON"},
		{"internal/dashboard/discovery", "internal/dashboard/httputil.WriteJSON"},
		{"internal/dashboard/ext/agentevents", "internal/dashboard/httputil.WriteJSON"},
		{"cmd/naozhi", "encoding/json.Marshal"},
		{"internal/session", "(*encoding/json.Encoder).Encode"},
	},
}

// egressSite is one conversion of an EventEntry-reaching value to an interface.
type egressSite struct {
	Pkg, File string
	Line      int
	Callee    string // receiving function, or return / assign / var / field / elem / send / conversion
	Form      string
	Verdict   string // projected | exempt | violation
}

// collectTypedViolations loads the module at root and runs the typed rules.
func collectTypedViolations(root string, report bool) ([]Violation, error) {
	prog, err := loadTyped(root)
	if err != nil {
		return nil, err
	}
	sites, vs := scanWireEgress(prog, wireEgress)
	if report {
		for _, s := range sites {
			fmt.Fprintf(os.Stderr, "wire_egress %-10s %s:%d %s %s\n", s.Verdict, s.File, s.Line, s.Callee, s.Form)
		}
	}
	return vs, nil
}

// scanWireEgress returns every conversion site and the violations among them,
// plus dead exemptions, unlisted generic instantiations and missing sentinels.
func scanWireEgress(prog *typedProgram, rules wireEgressRules) ([]egressSite, []Violation) {
	w := &egressWalker{prog: prog, rules: rules, exemptHits: map[int]int{}, genericHits: map[string]int{}}
	for _, p := range prog.Pkgs {
		w.pkg = p
		for _, f := range p.Files {
			w.walkFile(f)
		}
		w.checkInstances()
	}
	sort.Slice(w.sites, func(i, j int) bool {
		if w.sites[i].File != w.sites[j].File {
			return w.sites[i].File < w.sites[j].File
		}
		return w.sites[i].Line < w.sites[j].Line
	})
	var vs []Violation
	seen := map[[2]string]bool{}
	for _, s := range w.sites {
		seen[[2]string{s.Pkg, s.Callee}] = true
		if s.Verdict == "violation" {
			vs = append(vs, Violation{Rule: "wire_egress", File: s.File, Line: s.Line, Message: fmt.Sprintf(
				"%s receives %s, which JSON-reaches clievent.EventEntry without the wire projection: wrap it in clievent.ForWire / ForWireOne at the egress, or build the frame with wsproto.New*", s.Callee, s.Form)})
		}
	}
	sort.Slice(w.generic, func(i, j int) bool {
		return w.generic[i].File+fmt.Sprint(w.generic[i].Line) < w.generic[j].File+fmt.Sprint(w.generic[j].Line)
	})
	vs = append(vs, w.generic...)
	for i, ex := range rules.Exemptions {
		if w.exemptHits[i] == 0 {
			vs = append(vs, Violation{Rule: "wire_egress", File: "tools/lint-server-handlers/rule_wire_egress.go", Message: fmt.Sprintf(
				"exemption %s %s matches no site: a dead exemption, delete it", ex.Callee, ex.File)})
		}
	}
	for _, g := range rules.Generics {
		if w.genericHits[g] == 0 {
			vs = append(vs, Violation{Rule: "wire_egress", File: "tools/lint-server-handlers/rule_wire_egress.go", Message: fmt.Sprintf(
				"generic allowance %s matches no instantiation: delete it", g)})
		}
	}
	for _, s := range rules.Sentinels {
		if !seen[[2]string{s.Pkg, s.Callee}] {
			vs = append(vs, Violation{Rule: "wire_egress", File: s.Pkg, Message: fmt.Sprintf(
				"sentinel %s no longer receives any EventEntry-reaching value in %s: the rule has gone blind there, or the egress moved (update the sentinel)", s.Callee, s.Pkg)})
		}
	}
	return w.sites, vs
}

type egressFrame struct {
	sig          *types.Signature
	returnCallee string
}

type egressWalker struct {
	prog        *typedProgram
	rules       wireEgressRules
	pkg         *typedPkg
	frames      []egressFrame
	body        *ast.BlockStmt // outermost function body: the scope a traced local must live in
	poolNew     map[*ast.FuncLit]bool
	sites       []egressSite
	generic     []Violation
	exemptHits  map[int]int
	genericHits map[string]int
}

func (w *egressWalker) info() *types.Info { return w.pkg.Info }

func (w *egressWalker) walkFile(f *ast.File) {
	w.poolNew = map[*ast.FuncLit]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if cl, ok := n.(*ast.CompositeLit); ok && isNamed(w.info().Types[cl].Type, "sync", "Pool") {
			for _, el := range cl.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "New" {
						if fl, ok := kv.Value.(*ast.FuncLit); ok {
							w.poolNew[fl] = true
						}
					}
				}
			}
		}
		return true
	})
	ast.Inspect(f, w.visit)
}

func (w *egressWalker) visit(n ast.Node) bool {
	info := w.info()
	switch x := n.(type) {
	case *ast.FuncDecl:
		if x.Body == nil {
			return false
		}
		w.enter(info.Defs[x.Name].Type().(*types.Signature), "return", x.Body)
		ast.Inspect(x.Body, w.visit)
		w.leave()
		return false
	case *ast.FuncLit:
		callee := "return"
		if w.poolNew[x] {
			callee = poolNewCallee
		}
		w.enter(info.Types[x].Type.(*types.Signature), callee, x.Body)
		ast.Inspect(x.Body, w.visit)
		w.leave()
		return false
	case *ast.ReturnStmt:
		if len(w.frames) == 0 {
			return true
		}
		fr := w.frames[len(w.frames)-1]
		w.checkList(x.Results, tupleTypes(fr.sig.Results()), fr.returnCallee)
	case *ast.CallExpr:
		w.checkCall(x)
	case *ast.AssignStmt:
		if x.Tok == token.ASSIGN {
			var targets []types.Type
			for _, l := range x.Lhs {
				targets = append(targets, info.TypeOf(l))
			}
			w.checkList(x.Rhs, targets, "assign")
		}
	case *ast.ValueSpec:
		if x.Type != nil {
			t := info.TypeOf(x.Type)
			targets := make([]types.Type, len(x.Names))
			for i := range targets {
				targets[i] = t
			}
			w.checkList(x.Values, targets, "var")
		}
	case *ast.SendStmt:
		if ct, ok := info.TypeOf(x.Chan).Underlying().(*types.Chan); ok {
			w.check(x.Value, ct.Elem(), "send")
		}
	case *ast.CompositeLit:
		w.checkLiteral(x)
	}
	return true
}

func (w *egressWalker) enter(sig *types.Signature, returnCallee string, body *ast.BlockStmt) {
	w.frames = append(w.frames, egressFrame{sig: sig, returnCallee: returnCallee})
	if w.body == nil {
		w.body = body
	}
}

func (w *egressWalker) leave() {
	w.frames = w.frames[:len(w.frames)-1]
	if len(w.frames) == 0 {
		w.body = nil
	}
}

func (w *egressWalker) checkCall(x *ast.CallExpr) {
	info := w.info()
	ftv := info.Types[x.Fun]
	if ftv.IsType() {
		if len(x.Args) == 1 {
			w.check(x.Args[0], ftv.Type, "conversion")
		}
		return
	}
	if ftv.Type == nil {
		return
	}
	sig, ok := ftv.Type.Underlying().(*types.Signature)
	if !ok {
		return
	}
	params := sig.Params()
	n := params.Len()
	paramAt := func(i int) types.Type {
		switch {
		case sig.Variadic() && i >= n-1:
			if x.Ellipsis.IsValid() {
				return nil // f(xs...) passes the slice itself
			}
			return params.At(n - 1).Type().(*types.Slice).Elem()
		case i < n:
			return params.At(i).Type()
		}
		return nil
	}
	count := len(x.Args)
	if len(x.Args) == 1 { // f(g()) with a multi-value g
		if tup, ok := info.TypeOf(x.Args[0]).(*types.Tuple); ok {
			count = tup.Len()
		}
	}
	targets := make([]types.Type, count)
	for i := range targets {
		targets[i] = paramAt(i)
	}
	w.checkList(x.Args, targets, w.calleeName(x))
}

func (w *egressWalker) checkLiteral(x *ast.CompositeLit) {
	t := w.info().TypeOf(x)
	if t == nil {
		return
	}
	switch u := t.Underlying().(type) {
	case *types.Slice, *types.Array:
		elem := u.(interface{ Elem() types.Type }).Elem()
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			w.check(el, elem, "elem")
		}
	case *types.Map:
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				w.check(kv.Key, u.Key(), "map-key")
				w.check(kv.Value, u.Elem(), "map-value")
			}
		}
	case *types.Struct:
		for i, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok {
					if v, ok := w.info().Uses[id].(*types.Var); ok {
						w.check(kv.Value, v.Type(), "field")
					}
				}
			} else if i < u.NumFields() {
				w.check(el, u.Field(i).Type(), "field")
			}
		}
	}
}

// checkList pairs values with target types; a lone multi-value call on the
// right is spread over the targets.
func (w *egressWalker) checkList(values []ast.Expr, targets []types.Type, callee string) {
	if len(values) == 1 && len(targets) > 1 {
		if tup, ok := w.info().TypeOf(values[0]).(*types.Tuple); ok {
			for i := 0; i < tup.Len() && i < len(targets); i++ {
				w.record(values[0], tup.At(i).Type(), targets[i], callee, false)
			}
			return
		}
	}
	for i, v := range values {
		if i < len(targets) {
			w.check(v, targets[i], callee)
		}
	}
}

func (w *egressWalker) check(e ast.Expr, target types.Type, callee string) {
	if t := w.info().TypeOf(e); t != nil {
		if _, tuple := t.(*types.Tuple); !tuple {
			w.record(e, t, target, callee, true)
		}
	}
}

// record files a site when a value of type vt meets an interface target.
// single is false for one element of a multi-value call, which no projector
// produces.
func (w *egressWalker) record(e ast.Expr, vt, target types.Type, callee string, single bool) {
	if target == nil || !isInterface(target) || isInterface(vt) || !w.reaches(vt, map[types.Type]bool{}) {
		return
	}
	pos := w.prog.Fset.Position(e.Pos())
	s := egressSite{Pkg: w.pkg.Rel, File: w.relFile(pos.Filename), Line: pos.Line, Callee: callee, Form: w.form(e), Verdict: "violation"}
	if single && w.wireView(e, map[*types.Var]bool{}) {
		s.Verdict = "projected"
	} else if i := w.exemption(callee, s.File); i >= 0 {
		s.Verdict = "exempt"
		w.exemptHits[i]++
	}
	w.sites = append(w.sites, s)
}

func (w *egressWalker) exemption(callee, file string) int {
	for i, ex := range w.rules.Exemptions {
		if ex.Callee == callee && (ex.File == "" || ex.File == file) {
			return i
		}
	}
	return -1
}

// checkInstances flags generic instantiations whose type arguments reach
// EventEntry, unless the generic function is allowed.
func (w *egressWalker) checkInstances() {
	allowed := map[string]bool{}
	for _, g := range w.rules.Generics {
		allowed[g] = true
	}
	for id, inst := range w.info().Instances {
		reaches := false
		for i := 0; i < inst.TypeArgs.Len(); i++ {
			if w.reaches(inst.TypeArgs.At(i), map[types.Type]bool{}) {
				reaches = true
			}
		}
		if !reaches {
			continue
		}
		name := id.Name
		switch o := w.info().Uses[id].(type) {
		case *types.Func:
			name = relToModule(w.prog.Module, o.Origin().FullName())
		case *types.TypeName:
			if o.Pkg() != nil {
				name = relToModule(w.prog.Module, o.Pkg().Path()+"."+o.Name())
			}
		}
		if allowed[name] {
			w.genericHits[name]++
			continue
		}
		pos := w.prog.Fset.Position(id.Pos())
		w.generic = append(w.generic, Violation{Rule: "wire_egress", File: w.relFile(pos.Filename), Line: pos.Line, Message: fmt.Sprintf(
			"%s is instantiated with %s: inside a type parameter the value is invisible to wire_egress, so project with clievent.ForWire before the call or add a reasoned allowance", name, inst.TypeArgs.At(0))})
	}
}

// wireView reports whether e is a wire view (rules 1–3 in the file doc).
func (w *egressWalker) wireView(e ast.Expr, seen map[*types.Var]bool) bool {
	info := w.info()
	switch x := ast.Unparen(e).(type) {
	case *ast.CallExpr:
		if tv := info.Types[x.Fun]; tv.IsType() {
			return len(x.Args) == 1 && w.wireView(x.Args[0], seen)
		}
		callee := w.calleeName(x)
		for _, p := range w.rules.Projectors {
			if p == callee {
				return true
			}
		}
	case *ast.Ident:
		if info.Types[x].IsNil() {
			return true
		}
		if v, ok := info.Uses[x].(*types.Var); ok {
			return w.localWireView(v, seen)
		}
	case *ast.CompositeLit:
		return w.literalWireView(x, seen)
	case *ast.UnaryExpr:
		if lit, ok := ast.Unparen(x.X).(*ast.CompositeLit); ok && x.Op == token.AND {
			return w.literalWireView(lit, seen)
		}
	}
	return false
}

func (w *egressWalker) literalWireView(x *ast.CompositeLit, seen map[*types.Var]bool) bool {
	if len(x.Elts) == 0 {
		return true
	}
	t := w.info().TypeOf(x)
	if t == nil || w.isEntry(t) {
		return false
	}
	for i, el := range x.Elts {
		var vt types.Type
		val := el
		switch u := t.Underlying().(type) {
		case *types.Struct:
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				val = kv.Value
				if id, ok := kv.Key.(*ast.Ident); ok {
					if f, ok := w.info().Uses[id].(*types.Var); ok {
						vt = f.Type()
					}
				}
			} else if i < u.NumFields() {
				vt = u.Field(i).Type()
			}
		case *types.Slice, *types.Array, *types.Map:
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				val = kv.Value
			}
			vt = u.(interface{ Elem() types.Type }).Elem()
		default:
			return false
		}
		if vt == nil || (w.reaches(vt, map[types.Type]bool{}) && !w.wireView(val, seen)) {
			return false
		}
	}
	return true
}

// localWireView traces a local variable of the enclosing function (rule 3).
func (w *egressWalker) localWireView(v *types.Var, seen map[*types.Var]bool) bool {
	body := w.body
	if body == nil || v.Pos() < body.Pos() || v.Pos() >= body.End() {
		return false
	}
	if seen[v] {
		return true
	}
	seen[v] = true
	info := w.info()
	is := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && (info.Defs[id] == v || info.Uses[id] == v)
	}
	refType := isReference(v.Type())
	declared, ok := false, true
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, l := range s.Lhs {
				var rhs ast.Expr
				if len(s.Lhs) == len(s.Rhs) {
					rhs = s.Rhs[i]
				}
				switch {
				case is(l):
					if id := l.(*ast.Ident); info.Defs[id] == v {
						declared = true
					}
					ok = ok && rhs != nil && s.Tok != token.ADD_ASSIGN && w.wireView(rhs, seen)
				case is(rootIdent(l)) && w.reaches(info.TypeOf(l), map[types.Type]bool{}):
					ok = ok && rhs != nil && w.wireView(rhs, seen)
				}
			}
			for _, r := range s.Rhs {
				if refType && is(ast.Unparen(r)) {
					ok = false // an alias could be written through
				}
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				if info.Defs[id] != v {
					continue
				}
				declared = true
				if len(s.Values) > 0 && (len(s.Values) != len(s.Names) || !w.wireView(s.Values[i], seen)) {
					ok = false
				}
			}
			for _, r := range s.Values {
				if refType && is(ast.Unparen(r)) {
					ok = false
				}
			}
		case *ast.RangeStmt:
			for _, kv := range []ast.Expr{s.Key, s.Value} {
				if kv != nil && is(kv) {
					if id := kv.(*ast.Ident); info.Defs[id] == v {
						declared = true
					}
					ok = ok && w.wireView(s.X, seen)
				}
			}
		case *ast.UnaryExpr:
			if s.Op == token.AND && is(rootIdent(s.X)) {
				ok = false
			}
		case *ast.SelectorExpr:
			if sel := info.Selections[s]; sel != nil && sel.Kind() == types.MethodVal && is(rootIdent(s.X)) {
				if _, ptrRecv := sel.Obj().Type().(*types.Signature).Recv().Type().(*types.Pointer); ptrRecv {
					if _, isPtr := info.TypeOf(s.X).Underlying().(*types.Pointer); !isPtr {
						ok = false // implicit &v
					}
				}
			}
		}
		return true
	})
	return declared && ok // a parameter, closure ones included, is never declared in the body
}

// calleeName is the module-relative FullName of the called function, the name
// of a func-typed variable, or the expression text.
func (w *egressWalker) calleeName(c *ast.CallExpr) string {
	fun := ast.Unparen(c.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	var id *ast.Ident
	switch f := fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	}
	if id != nil {
		switch o := w.info().Uses[id].(type) {
		case *types.Func:
			return relToModule(w.prog.Module, o.Origin().FullName())
		case *types.Var, *types.Builtin:
			return o.Name()
		}
	}
	return types.ExprString(c.Fun)
}

func (w *egressWalker) form(e ast.Expr) string {
	s := types.ExprString(e)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return "`" + s + "`"
}

func (w *egressWalker) relFile(name string) string {
	if r, err := filepath.Rel(w.prog.Root, name); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(name)
}

func (w *egressWalker) isEntry(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Name() == entryType && n.Obj().Pkg() != nil &&
		relToModule(w.prog.Module, n.Obj().Pkg().Path()) == entryPkg
}

// reaches reports whether JSON encoding of a t value can reach an EventEntry.
func (w *egressWalker) reaches(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	if w.isEntry(t) {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		return w.reaches(u.Elem(), seen)
	case *types.Slice:
		return w.reaches(u.Elem(), seen)
	case *types.Array:
		return w.reaches(u.Elem(), seen)
	case *types.Map:
		return w.reaches(u.Elem(), seen)
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			f := u.Field(i)
			if (!f.Exported() && !f.Embedded()) || jsonTagName(u.Tag(i)) == "-" {
				continue
			}
			if w.reaches(f.Type(), seen) {
				return true
			}
		}
	}
	return false
}

func jsonTagName(tag string) string {
	name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
	return name
}

func isInterface(t types.Type) bool {
	if _, tp := t.(*types.TypeParam); tp {
		return false
	}
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

func isReference(t types.Type) bool {
	switch t.Underlying().(type) {
	case *types.Pointer, *types.Slice, *types.Map:
		return true
	}
	return false
}

func isNamed(t types.Type, pkg, name string) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Name() == name && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg
}

func rootIdent(e ast.Expr) ast.Expr {
	for {
		switch x := ast.Unparen(e).(type) {
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		default:
			return x
		}
	}
}

func tupleTypes(t *types.Tuple) []types.Type {
	out := make([]types.Type, t.Len())
	for i := range out {
		out[i] = t.At(i).Type()
	}
	return out
}
