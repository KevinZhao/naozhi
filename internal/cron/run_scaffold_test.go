package cron

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/metrics"
)

// newScaffoldFixture returns an inflight gate that has "won" the CAS (the
// precondition every runScaffold caller establishes) plus its finalizer.
func newScaffoldFixture() (*runInflight, *runFinalizer) {
	inflight := &runInflight{}
	inflight.running.Store(true)
	inflight.populate(runInflightView{RunID: "r1", Phase: PhaseQueued})
	return inflight, &runFinalizer{inflight: inflight}
}

// TestRunScaffold_NormalPath pins the happy-path envelope (#2174): the gauge
// is +1 while body runs and back to baseline afterwards, finalize released
// the CAS gate exactly once, and onPanic never fires.
//
// Not t.Parallel: metrics.CronRunInflight is a process-global gauge and the
// during-body assertion reads it as an absolute delta.
func TestRunScaffold_NormalPath(t *testing.T) {
	inflight, finalizer := newScaffoldFixture()
	base := metrics.CronRunInflight.Value()

	var during int64
	bodyCalls, panicCalls := 0, 0
	runScaffold{
		finalizer: finalizer,
		jobID:     "job-normal",
		onPanic:   func(any) { panicCalls++ },
	}.run(func() {
		bodyCalls++
		during = metrics.CronRunInflight.Value() - base
		if finalizer.done {
			t.Error("finalizer must not be finalized while body is running")
		}
	})

	if bodyCalls != 1 {
		t.Fatalf("body ran %d times, want 1", bodyCalls)
	}
	if during != 1 {
		t.Errorf("CronRunInflight during body = base%+d, want base+1", during)
	}
	if got := metrics.CronRunInflight.Value() - base; got != 0 {
		t.Errorf("CronRunInflight after run = base%+d, want base+0 (gauge must pair)", got)
	}
	if !finalizer.done {
		t.Error("finalizer.finalize() must have run in the scaffold defer")
	}
	if inflight.running.Load() {
		t.Error("CAS gate still held after normal completion")
	}
	if _, ok := inflight.snapshot(); ok {
		t.Error("inflight view still populated after finalize")
	}
	if panicCalls != 0 {
		t.Errorf("onPanic fired %d times on the normal path, want 0", panicCalls)
	}
}

// TestRunScaffold_BodyFinalizedViaFinishRun mirrors the real success path:
// finishRun already called finalize() inside body. The scaffold defer's
// finalize must then be an idempotent no-op (finalizer.done already true) —
// pinning that the scaffold never double-resets a slot a later run may have
// re-acquired (R246-GO-3 / #689).
func TestRunScaffold_BodyFinalizedViaFinishRun(t *testing.T) {
	inflight, finalizer := newScaffoldFixture()
	base := metrics.CronRunInflight.Value()

	runScaffold{finalizer: finalizer, jobID: "job-prefinalized"}.run(func() {
		finalizer.finalize()
		// Simulate run-B winning the freed slot before run-A's defer fires.
		inflight.running.Store(true)
		inflight.populate(runInflightView{RunID: "r2", Phase: PhaseQueued})
	})

	if got := metrics.CronRunInflight.Value() - base; got != 0 {
		t.Errorf("CronRunInflight after run = base%+d, want 0", got)
	}
	if !inflight.running.Load() {
		t.Error("scaffold defer clobbered run-B's CAS gate; finalize must be idempotent per finalizer")
	}
	if v, ok := inflight.snapshot(); !ok || v.RunID != "r2" {
		t.Errorf("scaffold defer clobbered run-B's inflight view: got %+v ok=%v", v, ok)
	}
}

// TestRunScaffold_PanicWithHook pins the replay-style panic path (#2064 /
// #2094): the panic is recovered, onPanic fires exactly once, and at the
// instant onPanic runs the slot is ALREADY finalized (finalize-before-
// broadcast). Afterwards the gauge is back to baseline.
func TestRunScaffold_PanicWithHook(t *testing.T) {
	inflight, finalizer := newScaffoldFixture()
	base := metrics.CronRunInflight.Value()

	panicCalls := 0
	var gotPanic any
	var finalizedAtHook, gateHeldAtHook bool
	var gaugeAtHook int64

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic escaped the scaffold despite onPanic being set: %v", r)
			}
		}()
		runScaffold{
			finalizer: finalizer,
			jobID:     "job-panic",
			onPanic: func(r any) {
				panicCalls++
				gotPanic = r
				finalizedAtHook = finalizer.done
				gateHeldAtHook = inflight.running.Load()
				gaugeAtHook = metrics.CronRunInflight.Value() - base
			},
		}.run(func() {
			panic("boom in body")
		})
	}()

	if panicCalls != 1 {
		t.Fatalf("onPanic fired %d times, want exactly 1", panicCalls)
	}
	if gotPanic != "boom in body" {
		t.Errorf("onPanic received %v, want the body's panic value", gotPanic)
	}
	if !finalizedAtHook {
		t.Error("onPanic ran before finalizer.finalize(): finalize-before-broadcast contract violated (#2094)")
	}
	if gateHeldAtHook {
		t.Error("CAS gate still held when onPanic ran; a concurrent CurrentRun could observe the run inflight after cron_run_ended")
	}
	// The gauge -1 lives in the OUTER defer, which fires after the recover
	// defer — so at hook time the run still counts as inflight for the
	// gauge. Pin that ordering so a reorder that moves Add(-1) ahead of the
	// broadcast is caught.
	if gaugeAtHook != 1 {
		t.Errorf("CronRunInflight at onPanic = base%+d, want base+1 (gauge -1 belongs to the outer defer)", gaugeAtHook)
	}
	if got := metrics.CronRunInflight.Value() - base; got != 0 {
		t.Errorf("CronRunInflight after panic = base%+d, want 0", got)
	}
	if !finalizer.done || inflight.running.Load() {
		t.Error("slot not released after panic path")
	}
}

// TestRunScaffold_PanicWithoutHookPropagates pins the executeOpt-style
// contract: with onPanic == nil the scaffold does NOT recover — the panic
// reaches the caller's own boundary (executeIfNotDeletedOrPaused / robfig
// Recover) — but the finalize + gauge defer still fires first, so the slot
// is released and the gauge balanced by the time the caller sees the panic.
func TestRunScaffold_PanicWithoutHookPropagates(t *testing.T) {
	inflight, finalizer := newScaffoldFixture()
	base := metrics.CronRunInflight.Value()

	var caught any
	var finalizedAtCatch, gateHeldAtCatch bool
	var gaugeAtCatch int64
	func() {
		defer func() {
			caught = recover()
			finalizedAtCatch = finalizer.done
			gateHeldAtCatch = inflight.running.Load()
			gaugeAtCatch = metrics.CronRunInflight.Value() - base
		}()
		runScaffold{finalizer: finalizer, jobID: "job-propagate"}.run(func() {
			panic("boom propagates")
		})
	}()

	if caught != "boom propagates" {
		t.Fatalf("caller recovered %v, want the body's panic to propagate when onPanic is nil", caught)
	}
	if !finalizedAtCatch {
		t.Error("finalizer must be finalized before the panic reaches the caller")
	}
	if gateHeldAtCatch {
		t.Error("CAS gate leaked across a propagated panic")
	}
	if gaugeAtCatch != 0 {
		t.Errorf("CronRunInflight when caller caught panic = base%+d, want 0", gaugeAtCatch)
	}
}

// TestRunScaffold_NilFinalizerSafe: finalize() is documented nil-safe; the
// scaffold must not NPE on a nil finalizer (defensive — every real caller
// passes a non-nil one).
func TestRunScaffold_NilFinalizerSafe(t *testing.T) {
	base := metrics.CronRunInflight.Value()
	ran := false
	runScaffold{jobID: "job-nil"}.run(func() { ran = true })
	if !ran {
		t.Fatal("body did not run")
	}
	if got := metrics.CronRunInflight.Value() - base; got != 0 {
		t.Errorf("CronRunInflight after run = base%+d, want 0", got)
	}
}

// TestRunScaffold_InflightGaugeSingleHome is the R202606-ARCH-7 (#2174)
// contract guard against re-hand-copying the envelope: within internal/cron
// (non-test sources) a call metrics.CronRunInflight.Add(...) may appear ONLY
// in run_scaffold.go, and there exactly once as +1 and once as -1. A new run
// path that bumps the gauge itself instead of going through runScaffold.run
// fails here. AST-based so comments mentioning the call do not count.
func TestRunScaffold_InflightGaugeSingleHome(t *testing.T) {
	t.Parallel()
	const home = "run_scaffold.go"

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no .go files found; test must run from the internal/cron package dir")
	}

	var offenders []string
	homeSeen := false
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		args := inflightGaugeAddArgs(file)
		if len(args) == 0 {
			continue
		}
		if f != home {
			offenders = append(offenders, f)
			continue
		}
		homeSeen = true
		var plus, minus int
		for _, a := range args {
			switch a {
			case "1":
				plus++
			case "-1":
				minus++
			default:
				t.Errorf("%s: unexpected CronRunInflight.Add argument %q; only literal 1 / -1 allowed", f, a)
			}
		}
		if plus != 1 || minus != 1 {
			t.Errorf("%s: want exactly one Add(1) and one Add(-1), got +1×%d / -1×%d", f, plus, minus)
		}
	}
	if !homeSeen {
		t.Errorf("%s does not bump metrics.CronRunInflight; the scaffold must own the gauge", home)
	}
	if len(offenders) > 0 {
		t.Errorf("metrics.CronRunInflight.Add outside %s in %v — run bodies must go through runScaffold.run instead of hand-copying the gauge/finalizer envelope (R202606-ARCH-7 #2174)", home, offenders)
	}
}

// inflightGaugeAddArgs returns the source text of the single argument of every
// `metrics.CronRunInflight.Add(<arg>)` call expression in file.
func inflightGaugeAddArgs(file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Add" {
			return true
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != "CronRunInflight" {
			return true
		}
		pkg, ok := inner.X.(*ast.Ident)
		if !ok || pkg.Name != "metrics" {
			return true
		}
		if len(call.Args) != 1 {
			out = append(out, "<bad arity>")
			return true
		}
		out = append(out, exprText(call.Args[0]))
		return true
	})
	return out
}

// exprText renders the literal forms the contract accepts (1 / -1); anything
// else is returned as a marker so the test reports it.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Value
	case *ast.UnaryExpr:
		if lit, ok := v.X.(*ast.BasicLit); ok {
			return v.Op.String() + lit.Value
		}
	}
	return "<non-literal>"
}
