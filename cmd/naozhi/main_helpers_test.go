package main

import (
	"os"
	"strings"
	"testing"
)

func TestChatIDSuffix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short_seven", "oc_abcde", "oc_abcde"}, // ≤8 bytes passes through
		{"exact_eight", "12345678", "12345678"},
		{"nine_chars", "123456789", "…23456789"},
		{"feishu_chat_id", "oc_9dcbfd8307c7a4c1e111f163aa47fd5d", "…aa47fd5d"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chatIDSuffix(tc.in); got != tc.want {
				t.Fatalf("chatIDSuffix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMain_DispatchesDiagnosticSubcommands pins the Round 130 audit: the
// `naozhi` CLI must keep `doctor` as a first-class subcommand and the
// production entry path must register pprof on startup. Both of these
// serve on-call runbooks and ship with dedicated docs / tests; a silent
// removal (e.g. during a CLI refactor that collapses the dispatch
// switch) would leave the runbook steps broken with no error until
// someone paged at 3am. This test fails immediately on such a drop.
//
// The assertion reads main.go source — Go's os.Args-driven dispatch
// isn't reflectable at runtime without invoking the process, so a
// string-level contract is the simplest way to catch the regression.
// Keep the regex tolerant to whitespace/comment additions.
func TestMain_DispatchesDiagnosticSubcommands(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(data)

	// doctor subcommand must still be in the dispatch switch. The
	// case body is short enough that pinning the exact two lines
	// (case + runDoctor) makes reordering / renaming obvious.
	if !strings.Contains(src, `case "doctor":`) {
		t.Error(`main.go dispatch missing "case \"doctor\":" — runDoctor would be unreachable from the CLI`)
	}
	if !strings.Contains(src, "runDoctor(os.Args[2:])") {
		t.Error(`main.go dispatch missing runDoctor call — "naozhi doctor" args would not forward`)
	}
}

// TestDashboardWiring_RegistersPprof pins that the server startup path
// still calls registerPprof — the pprof endpoints are defense-in-depth
// for memory / goroutine leak triage and the runbook at
// docs/ops/pprof.md tells operators to curl /api/debug/pprof/*. A PR
// that collapses or renames the wiring would silently break those
// commands; this test catches it.
//
// Reading dashboard.go source instead of reflection keeps the
// contract narrow: a caller that moves the wiring elsewhere just
// needs to update the test's expected file/token pair.
func TestDashboardWiring_RegistersPprof(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../internal/server/dashboard.go")
	if err != nil {
		t.Fatalf("read dashboard.go: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "s.registerPprof()") {
		t.Error("internal/server/dashboard.go must call s.registerPprof() during server startup — docs/ops/pprof.md depends on it")
	}
}
