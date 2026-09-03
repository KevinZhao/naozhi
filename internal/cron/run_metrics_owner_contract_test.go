package cron

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestRunMetricsOwnerContract pins #2173 (R202606-ARCH-4): every per-run
// lifecycle counter in internal/cron has exactly one owner function, and no
// caller of finishRun bumps a per-state counter by hand.
//
// Why an AST test and not a review rule: the sandbox counters were hand-bumped
// at three call sites (finishSandboxRunWith, both reconcileOneSandboxOrphan
// branches), each of which had to remember what finishRun → bumpRunStateMetrics
// had already counted. That produced R20260613-GOLANG-002 (timed-out
// double-count), R20260614-LOGIC-9 (missing timeout bucket) and
// R20260614-GO-001 / R20260613-CR-2 (orphan under-count). A fourth call site
// would re-open the same class of drift; this test makes it a compile-adjacent
// failure instead of a metrics regression noticed weeks later.
//
// The allowlist is deliberately explicit per counter. Adding an owner is a
// design decision — update the map AND the godoc on bumpRunStateMetrics.
func TestRunMetricsOwnerContract(t *testing.T) {
	// counter → set of "file.go:FuncName" allowed to call metrics.<counter>.Add.
	allowed := map[string][]string{
		// Per-state buckets: bumpRunStateMetrics is the single owner. The
		// sandbox_replay.go exception is the panic-recover path in
		// dispatchReplay (#2223) which bypasses finishRun entirely; folding
		// it into finishRun is deferred to the #2174 run-goroutine scaffold PR.
		"CronRunSucceededTotal": {"scheduler_callbacks.go:bumpRunStateMetrics"},
		"CronRunFailedTotal":    {"scheduler_callbacks.go:bumpRunStateMetrics", "sandbox_replay.go:dispatchReplay"},
		"CronRunSkippedTotal":   {"scheduler_callbacks.go:bumpRunStateMetrics"},
		"CronRunTimedOutTotal":  {"scheduler_callbacks.go:bumpRunStateMetrics"},
		"CronRunCanceledTotal":  {"scheduler_callbacks.go:bumpRunStateMetrics"},
		// Sandbox-specific per-state buckets: same owner, gated by the
		// finishArgs.sandbox bool. No caller may bump these directly.
		"CronSandboxRunFailedTotal":   {"scheduler_callbacks.go:bumpRunStateMetrics", "sandbox_replay.go:dispatchReplay"},
		"CronSandboxRunTimedOutTotal": {"scheduler_callbacks.go:bumpRunStateMetrics"},
		// Lifecycle pair. emitRunStarted / finishRun own the live paths;
		// finishOrphanRun is the metrics-only mirror for a reconciled orphan
		// whose job no longer exists (no broadcast, so it cannot route through
		// emitRunStarted/finishRun without emitting a phantom lifecycle).
		"CronRunStartedTotal": {"scheduler_callbacks.go:emitRunStarted", "sandbox_pending.go:finishOrphanRun"},
		"CronRunEndedTotal":   {"scheduler_finish.go:finishRun", "sandbox_pending.go:finishOrphanRun", "sandbox_replay.go:dispatchReplay"},
	}

	got := collectMetricAddSites(t, ".", allowed)

	for counter, owners := range allowed {
		want := append([]string(nil), owners...)
		sort.Strings(want)
		have := got[counter]
		sort.Strings(have)
		// Positive direction first: every allowlisted owner must STILL bump the
		// counter. Without this, deleting the bump from bumpRunStateMetrics
		// would read as "no unexpected sites" and pass — the allowlist would
		// rot into documentation of code that no longer exists.
		for _, owner := range want {
			found := false
			for _, h := range have {
				if h == owner {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("metrics.%s.Add: allowlisted owner %s no longer bumps it — either the owner lost the bump (regression) or the allowlist is stale", counter, owner)
			}
		}
		// Negative direction: no site outside the allowlist.
		if strings.Join(have, ",") != strings.Join(want, ",") {
			t.Errorf("metrics.%s.Add call sites drifted\n  have: %v\n  want: %v\n"+
				"— per-state / lifecycle counters are owned by bumpRunStateMetrics, emitRunStarted, finishRun "+
				"and finishOrphanRun only (#2173). Route the new path through finishRun instead of bumping by hand.",
				counter, have, want)
		}
	}
}

// collectMetricAddSites parses every non-test .go file in dir and returns, for
// each counter in the allowlist keys, the sorted unique set of
// "file.go:EnclosingFunc" locations that call metrics.<counter>.Add(...).
// Closures are attributed to their enclosing top-level FuncDecl (that is the
// unit of ownership a reviewer reasons about).
func collectMetricAddSites(t *testing.T, dir string, tracked map[string][]string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := map[string]map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			owner := name + ":" + fd.Name.Name
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				counter, ok := metricsAddCounter(call)
				if !ok {
					return true
				}
				if _, tracked := tracked[counter]; !tracked {
					return true
				}
				if found[counter] == nil {
					found[counter] = map[string]bool{}
				}
				found[counter][owner] = true
				return true
			})
		}
	}
	out := map[string][]string{}
	for counter, owners := range found {
		for o := range owners {
			out[counter] = append(out[counter], o)
		}
		sort.Strings(out[counter])
	}
	return out
}

// metricsAddCounter recognises `metrics.<Counter>.Add(...)` and returns
// <Counter>.
func metricsAddCounter(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Add" {
		return "", false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := inner.X.(*ast.Ident)
	if !ok || pkg.Name != "metrics" {
		return "", false
	}
	return inner.Sel.Name, true
}
