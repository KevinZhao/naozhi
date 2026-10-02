package contractjs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
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

// TestContractJS_KnownAnchors pins a handful of load-bearing entries so the
// generator cannot silently produce an empty or misshapen file that still
// byte-matches a broken committed copy.
func TestContractJS_KnownAnchors(t *testing.T) {
	t.Parallel()
	out, err := Build(filepath.Join("..", "..", "internal", "server", "testdata", "routes.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []string{
		"sessions_update: 'sessions_update'", // WS enum
		"run_started: 'run_started'",         // WS enum, unified run frames (#2540)
		"subscribe: 'subscribe'",             // WS enum, inbound (send-side check reads these)
		"sessions: '/api/sessions'",          // API table
		"export const NZ_CONTRACT = {",       // the module export every consumer imports
	} {
		if !strings.Contains(out, anchor) {
			t.Errorf("contract.js lacks anchor %q", anchor)
		}
	}
}

// TestContractJS_EventTypeEnums pins each generated kind list to the clievent
// accessor it must come from. The golden above only proves contract.js matches
// Build; this proves Build put the right column under the right name, so a
// swapped or hand-typed list cannot ride through a regenerate.
func TestContractJS_EventTypeEnums(t *testing.T) {
	t.Parallel()
	out, err := Build(filepath.Join("..", "..", "internal", "server", "testdata", "routes.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"EVENT_TYPE":           clievent.AllKinds(),
		"EVENT_TYPE_INTERNAL":  clievent.InternalKinds(),
		"EVENT_TYPE_MD_IGNORE": clievent.MarkdownIgnoreKinds(),
	} {
		if len(want) == 0 {
			t.Fatalf("clievent returned no kinds for %s", name)
		}
		line := "    " + name + ": ['" + strings.Join(want, "', '") + "'],\n"
		if !strings.Contains(out, line) {
			t.Errorf("contract.js lacks %q", line)
		}
	}
}
