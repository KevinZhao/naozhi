package contractjs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/planner/agent"
)

// TestContractJS_Current rebuilds contract.js and byte-compares it against
// the committed file: any struct-tag, route or wsproto change without
// `go run ./tools/gen-contract` fails here. This single test replaces the
// hand-written field lists of the old *_shape_test.go files as the drift
// gate — delete a json tag (#2476's stableKey) and this goes red before any
// Playwright run.
func TestContractJS_Current(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	want, err := Build(filepath.Join(root, "internal", "server", "testdata", "routes.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "internal", "server", "static", "contract.js"))
	if err != nil {
		t.Fatalf("read contract.js (run `go run ./tools/gen-contract`): %v", err)
	}
	if string(got) != want {
		t.Error("contract.js is stale — run `go run ./tools/gen-contract` and commit the result")
	}
}

// TestContractJS_Anchors pins load-bearing entries, so the generator cannot
// produce an empty or misshapen file that still byte-matches a broken
// committed copy, and each generated list to the Go accessor it must come
// from, so a swapped or hand-typed list cannot ride through a regenerate.
func TestContractJS_Anchors(t *testing.T) {
	t.Parallel()
	out, err := Build(filepath.Join("..", "..", "internal", "server", "testdata", "routes.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	list := func(name string, items []string) string {
		if len(items) == 0 {
			t.Fatalf("no items for %s", name)
		}
		return "    " + name + ": ['" + strings.Join(items, "', '") + "'],\n"
	}
	for _, anchor := range []string{
		"sessions_update: 'sessions_update'",         // WS enum
		"run_started: 'run_started'",                 // WS enum, unified run frames (#2540)
		"subscribe: 'subscribe'",                     // WS enum, inbound (send-side check reads these)
		"sessions: '/api/sessions'",                  // API table
		"export const NZ_CONTRACT = Object.freeze({", // the export every consumer imports
		list("EVENT_TYPE", clievent.AllKinds()),
		list("EVENT_TYPE_INTERNAL", clievent.InternalKinds()),
		list("EVENT_TYPE_MD_IGNORE", clievent.MarkdownIgnoreKinds()),
		list("EVENT_TYPE_NO_BUBBLE", clievent.NoBubbleKinds()),
		list("STARTUP_FAILURE_CLASS", clierr.AllExitClassWires()),
		list("PLANNER_TURN_DYNAMIC", agent.DynamicSources()),
		list("PLANNER_TURN_SHIFT", []string{agent.SourceShift.Entry()}),
		"  DEATH_REASON_PREFIX: { CODE: '" + cliinfo.DeathReasonCLIExitedCodePrefix +
			"', SIGNAL: '" + cliinfo.DeathReasonCLIExitedSignalPrefix + "' },\n",
	} {
		if !strings.Contains(out, anchor) {
			t.Errorf("contract.js lacks %q", anchor)
		}
	}
}
