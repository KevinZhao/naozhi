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
// gaining/losing a call) would still be caught.
func markerCallSrc(pkgName string, n int) string {
	names := []string{"WithPassthrough", "IsPassthrough", "WithUrgent", "IsUrgent"}
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nfunc markerCalls() {\n", pkgName)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "\t_ = %s(nil)\n", names[i%len(names)])
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

// writeTurnBoundaryRoot lays out root/dispatch, root/server and (if turnFiles
// is non-nil) root/turn, and returns root. serverPkg for scanTurnBoundary is
// always filepath.Join(root, "server").
func writeTurnBoundaryRoot(t *testing.T, dispatchFiles, serverFiles, turnFiles map[string]string) string {
	t.Helper()
	root := t.TempDir()
	write := func(sub string, files map[string]string) {
		if files == nil {
			return
		}
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
	write("dispatch", dispatchFiles)
	write("server", serverFiles)
	write("turn", turnFiles)
	return root
}

// cleanFixture splits each baseline exactly between a dispatch and a server
// file, proving the rule sums across both directories rather than reading
// only one.
func cleanFixture(t *testing.T) string {
	t.Helper()
	dMarker, sMarker := 3, 3 // turnCtxMarkerBaseline == 6
	dSlash, sSlash := 6, 5   // turnSlashLiteralBaseline == 11
	dQueue, sQueue := 2, 2   // turnQueueEscapeBaseline == 4
	return writeTurnBoundaryRoot(t,
		map[string]string{
			"d_marker.go": markerCallSrc("dispatch", dMarker),
			"d_value.go":  withValueSrc("dispatch", turnCtxWithValueBaseline), // all 1 here
			"d_queue.go":  queueEscapeSrc("dispatch", dQueue),
			"d_slash.go":  slashLiteralSrc("dispatch", dSlash),
		},
		map[string]string{
			"s_marker.go": markerCallSrc("server", sMarker),
			"s_queue.go":  queueEscapeSrc("server", sQueue),
			"s_slash.go":  slashLiteralSrc("server", sSlash),
		},
		nil,
	)
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
	extra := filepath.Join(root, "dispatch", "d_extra_marker.go")
	if err := os.WriteFile(extra, []byte(markerCallSrc("dispatch", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if !hasRatchetViolation(vs, "turnCtxMarkerBaseline") {
		t.Fatalf("extra marker call not reported: %+v", vs)
	}
}

// One extra .Enqueue( in the server directory trips G-b (mutation: "在
// server 文件里加一次 .Enqueue( → G-b 失败").
func TestScanTurnBoundary_ExtraQueueEscape(t *testing.T) {
	t.Parallel()
	root := cleanFixture(t)
	extra := filepath.Join(root, "server", "s_extra_queue.go")
	if err := os.WriteFile(extra, []byte(queueEscapeSrc("server", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
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
	extra := filepath.Join(root, "server", "s_extra_slash.go")
	src := "package server\n\nfunc extraSlash(s string) bool { return s == \"/urgent \" }\n"
	if err := os.WriteFile(extra, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if !hasRatchetViolation(vs, "turnSlashLiteralBaseline") {
		t.Fatalf("extra slash literal not reported: %+v", vs)
	}
}

// One extra context.WithValue( call trips G-a's second slice.
func TestScanTurnBoundary_ExtraContextWithValue(t *testing.T) {
	t.Parallel()
	root := cleanFixture(t)
	extra := filepath.Join(root, "server", "s_extra_value.go")
	if err := os.WriteFile(extra, []byte(withValueSrc("server", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if !hasRatchetViolation(vs, "turnCtxWithValueBaseline") {
		t.Fatalf("extra context.WithValue call not reported: %+v", vs)
	}
}

// The count dropping without the baseline following it fails just as loudly
// as a rise (mutation: "计数降了但基线没改 → 双向检查失败") — delete one of the
// two files carrying ctx marker calls instead of lowering the constant.
func TestScanTurnBoundary_LoweredCountWithoutLoweredBaseline(t *testing.T) {
	t.Parallel()
	root := writeTurnBoundaryRoot(t,
		map[string]string{
			"d_marker.go": markerCallSrc("dispatch", 3),
			"d_value.go":  withValueSrc("dispatch", turnCtxWithValueBaseline),
			"d_queue.go":  queueEscapeSrc("dispatch", 2),
			"d_slash.go":  slashLiteralSrc("dispatch", 6),
		},
		map[string]string{
			// Only 2 marker calls here instead of 3: total is 5, one below
			// turnCtxMarkerBaseline (6).
			"s_marker.go": markerCallSrc("server", 2),
			"s_queue.go":  queueEscapeSrc("server", 2),
			"s_slash.go":  slashLiteralSrc("server", 5),
		},
		nil,
	)
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	v, ok := findRatchetViolation(vs, "turnCtxMarkerBaseline")
	if !ok {
		t.Fatalf("lowered count did not fail: %+v", vs)
	}
	if !strings.Contains(v.Message, "lower turnCtxMarkerBaseline to 5") {
		t.Errorf("message = %q, want it to say lower turnCtxMarkerBaseline to 5", v.Message)
	}
}

// G-a scans internal/turn (if present); G-b and G-d deliberately do not —
// a marker call inside turn is still reported, but an Enqueue call or a
// slash literal legitimately living there must not be.
func TestScanTurnBoundary_TurnDirectoryGaIncludedGbdExcluded(t *testing.T) {
	t.Parallel()
	root := writeTurnBoundaryRoot(t,
		map[string]string{
			"d_marker.go": markerCallSrc("dispatch", 3),
			"d_value.go":  withValueSrc("dispatch", turnCtxWithValueBaseline),
			"d_queue.go":  queueEscapeSrc("dispatch", 2),
			"d_slash.go":  slashLiteralSrc("dispatch", 6),
		},
		map[string]string{
			"s_marker.go": markerCallSrc("server", 2), // one short on purpose
			"s_queue.go":  queueEscapeSrc("server", 2),
			"s_slash.go":  slashLiteralSrc("server", 5),
		},
		map[string]string{
			// Legitimate turn-side code: one marker call (G-a must count it,
			// bringing the total back to the baseline of 6), plus the queue's
			// own Enqueue/DoneOrDrain implementation and turn/parse.go's
			// slash literals, neither of which G-b/G-d may count.
			"t_marker.go": markerCallSrc("turn", 1),
			"parse.go":    slashLiteralSrc("turn", 20),
			"queue.go":    queueEscapeSrc("turn", 20),
		},
	)
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if _, ok := findRatchetViolation(vs, "turnCtxMarkerBaseline"); ok {
		t.Errorf("turn's marker call was not counted into G-a: %+v", vs)
	}
	if v, ok := findRatchetViolation(vs, "turnQueueEscapeBaseline"); ok {
		t.Errorf("turn's own Enqueue/DoneOrDrain calls were counted by G-b: %+v", v)
	}
	if v, ok := findRatchetViolation(vs, "turnSlashLiteralBaseline"); ok {
		t.Errorf("turn/parse.go's slash literals were counted by G-d: %+v", v)
	}
}

// internal/turn absent (as it is until #2897 T3004-C1) is scanned as empty,
// not an error — the baseline fixture above already covers this (it passes
// no turnFiles), this test pins that the directory is allowed to not exist
// at all on disk.
func TestScanTurnBoundary_TurnDirectoryAbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	root := cleanFixture(t)
	if _, err := os.Stat(filepath.Join(root, "turn")); err == nil {
		t.Fatal("fixture unexpectedly created a turn directory")
	}
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if len(vs) != 0 {
		t.Fatalf("absent turn directory produced violations: %+v", vs)
	}
}

// dispatch is required: unlike the optional turn/dashboard directories, a
// missing dispatch directory is reported, not silently scanned as empty — a
// misconfigured -server-pkg must not be able to narrow G-a/G-b/G-d's scope.
func TestScanTurnBoundary_MissingDispatchDirectoryErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir() // no dispatch/, no server/
	if err := os.MkdirAll(filepath.Join(root, "server"), 0o700); err != nil {
		t.Fatal(err)
	}
	vs := scanTurnBoundary(filepath.Join(root, "server"))
	if len(vs) != 1 || vs[0].Rule != "turn_boundary" {
		t.Fatalf("want exactly one turn_boundary error violation, got %+v", vs)
	}
	if !strings.Contains(vs[0].File, "dispatch") {
		t.Errorf("violation file = %q, want it to name the dispatch directory", vs[0].File)
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
