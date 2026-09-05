package contractjs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"sessions: '/api/sessions'",          // API table

		"spawn_diags: 'spawn_diags'",     // sessions F section (#2532)
		"config_sha256: 'config_sha256'", // health F section (#2538)
		"module.exports = NZ_CONTRACT",   // node consumer path
	} {
		if !strings.Contains(out, anchor) {
			t.Errorf("contract.js lacks anchor %q", anchor)
		}
	}
}
