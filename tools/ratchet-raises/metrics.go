package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// metric is one ratchet value. A key that exists only in head is a raise
// from zero when newIsRaise is set (a new coupling edge, a new exemption) and
// a new ratchet otherwise (a new baseline constant, a new JS file whose lines
// the TOTAL already counts). A key that exists only in base — the baseline
// lost the file that carried it, or the whole document — is a raise to -1
// when goneIsRaise is set: deleting the ratchet silently un-does it (#3025).
// anyChangeIsRaise raises on a value change in either direction, for
// metrics whose value is not itself ordered (a sha-derived int for a golden
// pin: a smaller number is not an improvement).
type metric struct {
	value            int64
	newIsRaise       bool
	goneIsRaise      bool
	anyChangeIsRaise bool
}

type metrics map[string]metric

// goBaselineName is the name of a baseline constant.
var goBaselineName = regexp.MustCompile(`[Bb]aseline`)

// goConsts reads every baseline constant in files (path → source), package
// level or local, into go:<dir>#<name>: moving one between files of a
// package is not a change, and losing one (renamed, made a var, moved to
// another package, its file deleted) is a raise to -1. A constant some code
// in its directory uses (goRefs) also gets go-ref:<dir>#<name>, a raise to -1
// once its last use goes, and go-skip:<dir> sums the skips (skipCalls) in the
// files declaring or using one, which it returns per directory. It returns one
// problem per constant whose value is not a plain integer literal (left
// unread) and per name repeated within a directory (read as the largest).
func goConsts(files map[string]string, into metrics) ([]string, map[string][]string, error) {
	var problems []string
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	read := map[string]map[string]bool{} // dir → constants read into go:
	declares := map[string]bool{}        // files declaring a baseline constant
	for _, p := range slices.Sorted(maps.Keys(files)) {
		if skipGoPath(p) {
			continue
		}
		f, err := parser.ParseFile(fset, p, files[p], parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return nil, nil, err
		}
		parsed[p] = f
		ast.Inspect(f, func(n ast.Node) bool {
			d, ok := n.(*ast.GenDecl)
			if !ok || d.Tok != token.CONST {
				return true
			}
			for _, spec := range d.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if !goBaselineName.MatchString(id.Name) {
						continue
					}
					declares[p] = true
					at := fset.Position(id.Pos())
					v, ok := intLiteral(vs, i)
					dir := path.Dir(p)
					key := "go:" + dir + "#" + id.Name
					if !ok {
						problems = append(problems, fmt.Sprintf("%s:%d: baseline constant %s must be a plain integer literal", p, at.Line, id.Name))
						continue
					}
					if prev, dup := into[key]; dup {
						problems = append(problems, fmt.Sprintf("%s:%d: baseline constant %s is declared twice in %s", p, at.Line, id.Name, dir))
						v = max(v, prev.value)
					}
					into[key] = metric{value: v, goneIsRaise: true}
					if read[dir] == nil {
						read[dir] = map[string]bool{}
					}
					read[dir][id.Name] = true
				}
			}
			return false
		})
	}
	uses := map[string]bool{}
	skips := map[string]int64{} // dir → skips in the files that declare or use a baseline
	skipFiles := map[string][]string{}
	for _, p := range slices.Sorted(maps.Keys(parsed)) {
		f := parsed[p]
		dir := path.Dir(p)
		if read[dir] == nil || buildConstrained(p, f) {
			continue
		}
		used := false
		goRefs(f, read[dir], func(name string) {
			uses[dir+"#"+name] = true
			used = true
		})
		if used || declares[p] {
			skips[dir] += skipCalls(f)
			skipFiles[dir] = append(skipFiles[dir], p)
		}
	}
	for k := range uses {
		into["go-ref:"+k] = metric{value: 1, goneIsRaise: true}
	}
	for dir, n := range skips {
		into["go-skip:"+dir] = metric{value: n}
	}
	return problems, skipFiles, nil
}

// skipCalls counts the Skip, Skipf and SkipNow selectors in f, called or
// taken as a method value, on any receiver. run() compares head against the
// base versions of the files head counts, so only skips a change adds to them
// raise. Not counted: a skip in a file that neither declares nor uses a
// baseline, even in a test calling a comparison helper declared elsewhere; a
// skip through a helper in another file, or in a same-file helper that was
// there before a new comparing test calls it; one in a build-constrained file.
// A renamed file reads as new, its skips a raise. A skip removed from one
// counted file offsets one added in another of the directory.
func skipCalls(f *ast.File) int64 {
	var n int64
	ast.Inspect(f, func(node ast.Node) bool {
		if s, ok := node.(*ast.SelectorExpr); ok {
			switch s.Sel.Name {
			case "Skip", "Skipf", "SkipNow":
				n++
			}
		}
		return true
	})
	return n
}

// skipsIn is skipCalls over src; a missing (empty) or unparsable source
// counts 0, so a doubt reports a raise rather than hiding one.
func skipsIn(p, src string) int64 {
	if src == "" {
		return 0
	}
	f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution)
	if err != nil {
		return 0
	}
	return skipCalls(f)
}

// goRefs calls use for each identifier in f named in names that is not
// declaring a var or const. On the right side of a blank assignment (_ = x,
// var _ = x) only the arguments of a call count: a bare value there only
// keeps an unused constant compiling. Matching is by name, not by object;
// and a use that never runs to compare against the constant (an early
// return, an always-true comparison, a helper nothing calls, a conversion
// like _ = int(x), a variable nothing reads) still counts.
func goRefs(f *ast.File, names map[string]bool, use func(name string)) {
	var visit func(ast.Node) bool
	calls := func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			ast.Inspect(c, visit)
			return false
		}
		return true
	}
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if slices.ContainsFunc(n.Lhs, func(e ast.Expr) bool { return !isBlank(e) }) {
				return true
			}
			for _, v := range n.Rhs {
				ast.Inspect(v, calls)
			}
			return false
		case *ast.ValueSpec:
			walk := calls
			if slices.ContainsFunc(n.Names, func(id *ast.Ident) bool { return !isBlank(id) }) {
				walk = visit
			}
			for _, v := range n.Values {
				ast.Inspect(v, walk)
			}
			return false
		case *ast.Ident:
			if names[n.Name] {
				use(n.Name)
			}
		}
		return true
	}
	ast.Inspect(f, visit)
}

func isBlank(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "_"
}

// buildConstrained reports whether the file at p builds only for some
// platforms or tags: a //go:build or // +build line above its package
// clause, or a _GOOS / _GOARCH name suffix (checked with go/build against a
// platform no suffix names, so the list stays the toolchain's own).
func buildConstrained(p string, f *ast.File) bool {
	for _, g := range f.Comments {
		if g.Pos() >= f.Package {
			break
		}
		for _, c := range g.List {
			if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
				return true
			}
		}
	}
	ctx := build.Context{GOOS: "none", GOARCH: "none", OpenFile: func(string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("package p\n")), nil
	}}
	ok, err := ctx.MatchFile(".", path.Base(p))
	return err != nil || !ok
}

// intLiteral is the value of the i-th name of vs when it is written as an
// integer literal (any base, digit separators allowed).
func intLiteral(vs *ast.ValueSpec, i int) (int64, bool) {
	if len(vs.Values) != len(vs.Names) {
		return 0, false
	}
	lit, ok := vs.Values[i].(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	v, err := strconv.ParseInt(lit.Value, 0, 64)
	return v, err == nil
}

// skipGoPath reports whether p is in a testdata or vendor tree, which holds
// fixtures and dependencies, not ratchets.
func skipGoPath(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == "testdata" || part == "vendor" {
			return true
		}
	}
	return false
}

// jsRatchet reads scripts/js-ratchet.baseline.json. lines, configureDeps,
// deadInjections, innerHTMLAssign, htmlInsert and lateBindings are gated only
// as their sum: moving code between files is a refactor, not a raise (S20a,
// #3026 D-S20-4); the other per-file metrics are keys of their own. A new
// file's metrics are new keys, so the totals and MAX.maxFnLines keep one from
// absorbing growth; js-ratchet --check still holds each file's own values.
// The "_global" entry is not a file: its metrics are GLOBAL.<name>.
func jsRatchet(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc map[string]map[string]int64
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("js-ratchet baseline: %w", err)
	}
	// A total exists only once some file carries its metric: a revision that
	// predates the metric has no such ratchet, rather than one at zero.
	totals := map[string]int64{}
	for file, ms := range doc {
		if file == jsGlobal {
			for name, v := range ms {
				totals["GLOBAL."+name] = v
			}
			continue
		}
		for name, v := range ms {
			switch name {
			case "lines", "configureDeps", "deadInjections", "innerHTMLAssign", "htmlInsert", "lateBindings":
				totals["TOTAL."+name] += v
				continue
			case "fnOver100":
				totals["TOTAL.fnOver100"] += v
			case "maxFnLines":
				totals["MAX.maxFnLines"] = max(totals["MAX.maxFnLines"], v)
			}
			into["js-ratchet:"+file+"."+name] = metric{value: v}
		}
	}
	// Deleting a file's metrics, or the whole document, must not silently
	// erase the sum/max it fed: a total only base holds is a raise to -1.
	for k, v := range totals {
		into["js-ratchet:"+k] = metric{value: v, goneIsRaise: true}
	}
	return nil
}

// jsGlobal is the js-ratchet baseline's cross-file entry (no static/ file
// can be named it: they all end in .js).
const jsGlobal = "_global"

// jsCaps reads scripts/js-ratchet.caps.json: the fail-closed caps that sit
// alongside js-ratchet.baseline.json (#3025 S19-0). maxFnLines.default and
// lines.<file> are ratchet constants (goneIsRaise: deleting the cap must not
// silently remove it). exempt / sideEffectLegacy / cycleLegacy are escape
// hatches (newIsRaise: a new entry loosens the gate; dropping one — the file
// got clean — is free, same as any other ratchet improvement). default is
// decoded as an int64, like every other ratchet value here: encoding/json
// then rejects 1e19 or 120.5 outright, where a float64 converted with
// int64(...) is implementation-defined out of range (amd64 gives MinInt64,
// so "default": 1e19 would read as a cap lowered below 120 and pass).
func jsCaps(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc struct {
		MaxFnLines struct {
			Default *int64   `json:"default"`
			Exempt  []string `json:"exempt"`
		} `json:"maxFnLines"`
		Lines            map[string]int64 `json:"lines"`
		SideEffectLegacy []string         `json:"sideEffectLegacy"`
		CycleLegacy      []string         `json:"cycleLegacy"`
		Leaves           []string         `json:"leaves"`
		// The lists js-ratchet's analysis reads (S20a): an allowed receiver
		// or a new shell root zeroes or frees counts; a dropped late-binding
		// table stops counting its writes; a legacy receiver (S20k) passes
		// the closed-receiver check.
		InjectionAllow    []string          `json:"injectionAllow"`
		InjectionLegacy   []string          `json:"injectionLegacy"`
		ShellRoots        []string          `json:"shellRoots"`
		LateBindingTables map[string]string `json:"lateBindingTables"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("js-ratchet caps: %w", err)
	}
	if doc.MaxFnLines.Default != nil {
		into["js-caps:maxFnLines.default"] = metric{value: *doc.MaxFnLines.Default, goneIsRaise: true}
	}
	for _, f := range doc.MaxFnLines.Exempt {
		into["js-caps:exempt:"+f] = metric{value: 1, newIsRaise: true}
	}
	for f, v := range doc.Lines {
		into["js-caps:lines."+f] = metric{value: v, goneIsRaise: true}
	}
	for _, f := range doc.SideEffectLegacy {
		into["js-caps:sideEffectLegacy:"+f] = metric{value: 1, newIsRaise: true}
	}
	for _, f := range doc.CycleLegacy {
		into["js-caps:cycleLegacy:"+f] = metric{value: 1, newIsRaise: true}
	}
	// A leaf may import only other leaves (S20a); dropping a file from the
	// list frees it to import anything, so a lost entry is the raise.
	for _, f := range doc.Leaves {
		into["js-caps:leaf:"+f] = metric{value: 1, goneIsRaise: true}
	}
	for _, a := range doc.InjectionAllow {
		into[capsSections["injectionAllow"]+a] = metric{value: 1, newIsRaise: true}
	}
	for _, a := range doc.InjectionLegacy {
		into[capsSections["injectionLegacy"]+a] = metric{value: 1, newIsRaise: true}
	}
	for _, f := range doc.ShellRoots {
		into[capsSections["shellRoots"]+f] = metric{value: 1, newIsRaise: true}
	}
	for name, f := range doc.LateBindingTables {
		into[capsSections["lateBindingTables"]+name+"="+f] = metric{value: 1, goneIsRaise: true}
	}
	return nil
}

// capsSections maps the caps.json lists that arrived after the document
// itself to their metric key prefix. A list base does not have yet is being
// created, so its first entries are recorded, not raised (run()), the same
// rule as for the whole document.
var capsSections = map[string]string{
	"injectionAllow":    "js-caps:injectionAllow:",
	"injectionLegacy":   "js-caps:injectionLegacy:",
	"shellRoots":        "js-caps:shellRoot:",
	"lateBindingTables": "js-caps:lateBindingTable:",
}

// goldenPins reads test/e2e/golden/pins.json: a map from golden fixture file
// to its sha256 (hex). Only the first 12 hex chars are kept, as an int64 —
// enough entropy to make a collision between two genuinely different
// renderings not worth engineering ratchet metrics around, and small enough
// to fit the same int64 every other metric uses. A changed or deleted pin is
// a raise; new pins are not newIsRaise here, so creating the file is free,
// and run() marks them newIsRaise once base already has a pins document.
func goldenPins(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc map[string]string
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("golden pins: %w", err)
	}
	for file, sha := range doc {
		short := sha
		if len(short) > 12 {
			short = short[:12]
		}
		v, err := strconv.ParseInt(short, 16, 64)
		if err != nil {
			return fmt.Errorf("golden pins: %s: bad sha %q: %w", file, sha, err)
		}
		into["golden:"+file] = metric{value: v, goneIsRaise: true, anyChangeIsRaise: true}
	}
	return nil
}

// jsDeps reads scripts/js-deps-baseline.json. Each cross-file reference in
// matrix and tdz is an edge that must not appear; typeofGuards and bridgeRefs
// are counts that must not grow.
func jsDeps(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc struct {
		Matrix       map[string]map[string][]string `json:"matrix"`
		TDZ          map[string]map[string][]string `json:"tdz"`
		TypeofGuards map[string]int64               `json:"typeofGuards"`
		BridgeRefs   map[string]int64               `json:"bridgeRefs"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("js-deps baseline: %w", err)
	}
	edges := func(kind string, m map[string]map[string][]string) {
		for from, tos := range m {
			for to, syms := range tos {
				for _, s := range syms {
					into["js-deps:"+kind+":"+from+">"+to+":"+s] = metric{value: 1, newIsRaise: true}
				}
			}
		}
	}
	edges("matrix", doc.Matrix)
	edges("tdz", doc.TDZ)
	for f, v := range doc.TypeofGuards {
		into["js-deps:typeofGuards:"+f] = metric{value: v, newIsRaise: true}
	}
	for f, v := range doc.BridgeRefs {
		into["js-deps:bridgeRefs:"+f] = metric{value: v, newIsRaise: true}
	}
	return nil
}

// exemptions reads tools/lint-server-handlers/exemptions.yaml. A new
// file_size entry, a higher current or limit, a later until date and a new
// handle_baseline entry each loosen the lint.
func exemptions(raw string, into metrics) error {
	if raw == "" {
		return nil
	}
	var doc struct {
		FileSize []struct {
			Path    string `yaml:"path"`
			Current int64  `yaml:"current"`
			Limit   int64  `yaml:"limit"`
			Until   string `yaml:"until"`
		} `yaml:"file_size"`
		HandleBaseline []string `yaml:"handle_baseline"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("exemptions: %w", err)
	}
	for _, e := range doc.FileSize {
		k := "exemptions:file_size:" + e.Path
		into[k] = metric{value: 1, newIsRaise: true}
		into[k+".current"] = metric{value: e.Current}
		into[k+".limit"] = metric{value: e.Limit}
		// 2027-03-31 → 20270331, so a later date is a larger number.
		d, _ := strconv.ParseInt(strings.ReplaceAll(e.Until, "-", ""), 10, 64)
		into[k+".until"] = metric{value: d}
	}
	for _, h := range doc.HandleBaseline {
		into["exemptions:handle_baseline:"+h] = metric{value: 1, newIsRaise: true}
	}
	return nil
}

// raise is one loosened ratchet value.
type raise struct {
	Gate string `json:"gate"`
	From int64  `json:"from"`
	To   int64  `json:"to"`
}

// raises lists every value that went up between base and head, sorted by
// gate. A key base held but head lost is a raise to -1 when base marked it
// goneIsRaise (#3025).
func raises(base, head metrics) []raise {
	var out []raise
	seen := make(map[string]bool, len(head))
	for k, h := range head {
		seen[k] = true
		b, existed := base[k]
		switch {
		case !existed && h.newIsRaise:
			out = append(out, raise{Gate: k, From: 0, To: h.value})
		case existed && h.value > b.value:
			out = append(out, raise{Gate: k, From: b.value, To: h.value})
		case existed && h.value < b.value && (h.anyChangeIsRaise || b.anyChangeIsRaise):
			out = append(out, raise{Gate: k, From: b.value, To: h.value})
		}
	}
	for k, b := range base {
		if seen[k] || !b.goneIsRaise {
			continue
		}
		out = append(out, raise{Gate: k, From: b.value, To: -1})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Gate < out[j].Gate })
	return out
}
