package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// markerCallSrc returns n calls to one of the four ctx marker functions,
// cycling through all four so a single-name regression (only one of them
// gaining/losing a call) would still be caught. qual "" writes the bare form
// dispatch uses on itself; otherwise the qualified form (dispatch.IsUrgent).
func markerCallSrc(pkgName, qual string, n int) string {
	names := []string{"WithPassthrough", "IsPassthrough", "WithUrgent", "IsUrgent"}
	if qual != "" {
		qual += "."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nfunc markerCalls() {\n", pkgName)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "\t_ = %s%s(nil)\n", qual, names[i%len(names)])
	}
	b.WriteString("}\n")
	return b.String()
}

// withValueSrc returns n context.WithValue( call sites.
func withValueSrc(pkgName string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nfunc withValueCalls() {\n", pkgName)
	for i := 0; i < n; i++ {
		b.WriteString("\t_ = context.WithValue(nil, nil, nil)\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// queueEscapeSrc returns n .Enqueue(/.DoneOrDrain( call sites, alternating.
func queueEscapeSrc(pkgName string, n int) string {
	names := []string{"Enqueue", "DoneOrDrain"}
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nfunc queueCalls(q *T) {\n", pkgName)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "\t_ = q.%s()\n", names[i%len(names)])
	}
	b.WriteString("}\n")
	return b.String()
}

// slashLiteralSrc returns n slash-command literal occurrences, cycling
// through all six forms.
func slashLiteralSrc(pkgName string, n int) string {
	lits := []string{"/new", "/new ", "/clear", "/clear ", "/urgent", "/urgent "}
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nfunc slashLiterals(s string) bool {\n\treturn ", pkgName)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(" ||\n\t\t")
		}
		fmt.Fprintf(&b, "s == %s", strconv.Quote(lits[i%len(lits)]))
	}
	if n == 0 {
		b.WriteString("false")
	}
	b.WriteString("\n}\n")
	return b.String()
}

// noiseSrc holds near-misses no check may count: a FuncDecl named like a
// marker, WithValue on a receiver other than context, another context call,
// a lookalike method name and a lookalike literal.
const noiseSrc = `package dispatch

func WithPassthrough(ctx any) any { return ctx }

func noise(x *T) {
	_ = x.WithValue(nil, nil, nil)
	_ = context.Background()
	_ = x.Enqueued()
	_ = "/news"
}
`

// writeTurnBoundaryRoot lays out root/<sub>/<name> for every entry of dirs
// and returns root. serverPkg for scanTurnBoundary is always
// filepath.Join(root, "server").
func writeTurnBoundaryRoot(t *testing.T, dirs map[string]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for sub, files := range dirs {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, src := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// baselineDirs splits each baseline between dispatch (bare marker calls) and
// server (qualified ones), proving the rule sums across both directories and
// recognises both call forms. sMarker == 2 is exactly every baseline (marker
// 1+2 == 3, slash 2+2 == 4, queue 1+1 == 2, WithValue 1).
// turn is a required directory, so every fixture carries one; this default
// file matches nothing and shifts no count.
func baselineDirs(sMarker int) map[string]map[string]string {
	return map[string]map[string]string{
		"dispatch": {
			"d_marker.go": markerCallSrc("dispatch", "", 1),
			"d_value.go":  withValueSrc("dispatch", turnCtxWithValueBaseline),
			"d_queue.go":  queueEscapeSrc("dispatch", 1),
			"d_slash.go":  slashLiteralSrc("dispatch", 2),
			"d_noise.go":  noiseSrc,
		},
		"server": {
			"s_marker.go": markerCallSrc("server", "dispatch", sMarker),
			"s_queue.go":  queueEscapeSrc("server", 1),
			"s_slash.go":  slashLiteralSrc("server", 2),
		},
		"turn": {
			"t_noop.go": "package turn\n",
		},
	}
}

func cleanFixture(t *testing.T) string {
	t.Helper()
	return writeTurnBoundaryRoot(t, baselineDirs(2))
}

func writeExtra(t *testing.T, root, sub, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, sub, name), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

// At exactly the four baselines, scanTurnBoundary reports nothing.
func TestScanTurnBoundary_CleanAtBaseline(t *testing.T) {
	t.Parallel()
	root := cleanFixture(t)
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if len(vs) != 0 {
		t.Fatalf("clean fixture reported %d violation(s): %+v", len(vs), vs)
	}
}

// One extra ctx marker call trips G-a's first slice (mutation: "fixture 里加
// 一次 WithPassthrough 调用 → G-a 失败").
func TestScanTurnBoundary_ExtraMarkerCall(t *testing.T) {
	t.Parallel()
	root := cleanFixture(t)
	writeExtra(t, root, "dispatch", "d_extra_marker.go", markerCallSrc("dispatch", "", 1))
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if !hasRatchetViolation(vs, "turnCtxMarkerBaseline") {
		t.Fatalf("extra marker call not reported: %+v", vs)
	}
}

// Each of the four marker names counts on its own: the baseline fixture is
// down to 3 calls, so it no longer cycles through all of them.
func TestScanTurnBoundary_EveryMarkerNameCounts(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"WithPassthrough", "IsPassthrough", "WithUrgent", "IsUrgent"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := cleanFixture(t)
			writeExtra(t, root, "server", "s_extra_marker.go",
				"package server\n\nfunc extraMarker() { _ = dispatch."+name+"(nil) }\n")
			vs := scanTurnBoundary(filepath.Join(root, "server"))
			if !hasRatchetViolation(vs, "turnCtxMarkerBaseline") {
				t.Fatalf("an extra %s call was not reported: %+v", name, vs)
			}
		})
	}
}

// One extra .Enqueue( in the server directory trips G-b (mutation: "在
// server 文件里加一次 .Enqueue( → G-b 失败").
func TestScanTurnBoundary_ExtraQueueEscape(t *testing.T) {
	t.Parallel()
	root := cleanFixture(t)
	writeExtra(t, root, "server", "s_extra_queue.go", queueEscapeSrc("server", 1))
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if !hasRatchetViolation(vs, "turnQueueEscapeBaseline") {
		t.Fatalf("extra queue escape call not reported: %+v", vs)
	}
}

// One extra "/urgent " literal trips G-d (mutation: "加一个 \"/urgent \" 字面量
// → G-d 失败").
func TestScanTurnBoundary_ExtraSlashLiteral(t *testing.T) {
	t.Parallel()
	root := cleanFixture(t)
	writeExtra(t, root, "server", "s_extra_slash.go",
		"package server\n\nfunc extraSlash(s string) bool { return s == \"/urgent \" }\n")
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if !hasRatchetViolation(vs, "turnSlashLiteralBaseline") {
		t.Fatalf("extra slash literal not reported: %+v", vs)
	}
}

// One extra context.WithValue( call trips G-a's second slice.
func TestScanTurnBoundary_ExtraContextWithValue(t *testing.T) {
	t.Parallel()
	root := cleanFixture(t)
	writeExtra(t, root, "server", "s_extra_value.go", withValueSrc("server", 1))
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if !hasRatchetViolation(vs, "turnCtxWithValueBaseline") {
		t.Fatalf("extra context.WithValue call not reported: %+v", vs)
	}
}

// The count dropping without the baseline following it fails just as loudly
// as a rise (mutation: "计数降了但基线没改 → 双向检查失败"): server carries 1
// marker call instead of 2, total 2, one below turnCtxMarkerBaseline.
func TestScanTurnBoundary_LoweredCountWithoutLoweredBaseline(t *testing.T) {
	t.Parallel()
	root := writeTurnBoundaryRoot(t, baselineDirs(1))
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	v, ok := findRatchetViolation(vs, "turnCtxMarkerBaseline")
	if !ok {
		t.Fatalf("lowered count did not fail: %+v", vs)
	}
	if !strings.Contains(v.Message, "lower turnCtxMarkerBaseline to 2") {
		t.Errorf("message = %q, want it to say lower turnCtxMarkerBaseline to 2", v.Message)
	}
}

// G-a scans internal/turn (if present); G-b and G-d deliberately do not —
// a marker call or context.WithValue inside turn is still reported, but an
// Enqueue call or a slash literal legitimately living there must not be.
func TestScanTurnBoundary_TurnDirectoryGaIncludedGbdExcluded(t *testing.T) {
	t.Parallel()
	dirs := baselineDirs(1) // one marker call short on purpose
	// turn's one marker call brings G-a back to its baseline of 3; the
	// queue's own implementation and turn/parse.go's literals must not count.
	dirs["turn"] = map[string]string{
		"t_marker.go": markerCallSrc("turn", "dispatch", 1),
		"t_value.go":  withValueSrc("turn", 1),
		"parse.go":    slashLiteralSrc("turn", 20),
		"queue.go":    queueEscapeSrc("turn", 20),
	}
	root := writeTurnBoundaryRoot(t, dirs)
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if _, ok := findRatchetViolation(vs, "turnCtxMarkerBaseline"); ok {
		t.Errorf("turn's marker call was not counted into G-a: %+v", vs)
	}
	if !hasRatchetViolation(vs, "turnCtxWithValueBaseline") {
		t.Errorf("turn's context.WithValue call was not counted into G-a: %+v", vs)
	}
	if v, ok := findRatchetViolation(vs, "turnQueueEscapeBaseline"); ok {
		t.Errorf("turn's own Enqueue/DoneOrDrain calls were counted by G-b: %+v", v)
	}
	if v, ok := findRatchetViolation(vs, "turnSlashLiteralBaseline"); ok {
		t.Errorf("turn/parse.go's slash literals were counted by G-d: %+v", v)
	}
}

// internal/upstream (the deferred third turn entry) is covered by G-a's
// marker slice and by G-b, but not by the context.WithValue slice or G-d.
func TestScanTurnBoundary_UpstreamMarkerAndQueueCounted(t *testing.T) {
	t.Parallel()
	dirs := baselineDirs(2)
	dirs["upstream"] = map[string]string{
		"u_marker.go": markerCallSrc("upstream", "dispatch", 1),
		"u_queue.go":  queueEscapeSrc("upstream", 1),
		"u_value.go":  withValueSrc("upstream", 5),
		"u_slash.go":  slashLiteralSrc("upstream", 5),
	}
	root := writeTurnBoundaryRoot(t, dirs)
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if !hasRatchetViolation(vs, "turnCtxMarkerBaseline") {
		t.Errorf("upstream's marker call was not counted by G-a: %+v", vs)
	}
	if !hasRatchetViolation(vs, "turnQueueEscapeBaseline") {
		t.Errorf("upstream's DoneOrDrain-style call was not counted by G-b: %+v", vs)
	}
	if v, ok := findRatchetViolation(vs, "turnCtxWithValueBaseline"); ok {
		t.Errorf("upstream's context.WithValue calls were counted: %+v", v)
	}
	if v, ok := findRatchetViolation(vs, "turnSlashLiteralBaseline"); ok {
		t.Errorf("upstream's slash literals were counted by G-d: %+v", v)
	}
}

// dispatch, server and turn are required: unlike the optional upstream
// directory, a missing one is reported, not silently scanned as empty — a
// misconfigured -server-pkg must not be able to narrow the rule's scope.
func TestScanTurnBoundary_MissingRequiredDirectoryErrors(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"dispatch", "server", "turn"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			dirs := baselineDirs(2)
			delete(dirs, missing)
			root := writeTurnBoundaryRoot(t, dirs)
			vs := scanTurnBoundary(filepath.Join(root, "server"))
			if len(vs) != 1 || vs[0].Rule != "turn_boundary" {
				t.Fatalf("want exactly one turn_boundary error violation, got %+v", vs)
			}
			if !strings.HasSuffix(vs[0].File, "/"+missing) {
				t.Errorf("violation file = %q, want it to name the %s directory", vs[0].File, missing)
			}
		})
	}
}

// hasRatchetViolation reports whether vs contains a violation naming
// constName (ratchetViolation always puts the constant name first in its
// message).
func hasRatchetViolation(vs []Violation, constName string) bool {
	_, ok := findRatchetViolation(vs, constName)
	return ok
}

func findRatchetViolation(vs []Violation, constName string) (Violation, bool) {
	for _, v := range vs {
		if v.Rule == "turn_boundary" && strings.HasPrefix(v.Message, constName+":") {
			return v, true
		}
	}
	return Violation{}, false
}
