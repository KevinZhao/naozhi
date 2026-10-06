package contractjs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/wsproto"
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
		"sessions_update: 'sessions_update'",                               // WS enum
		"run_started: 'run_started'",                                       // WS enum, unified run frames (#2540)
		"subscribe: 'subscribe'",                                           // WS enum, inbound (send-side check reads these)
		"sessions: '/api/sessions'",                                        // API table
		"export const NZ_CONTRACT = Object.freeze(/** @type {const} */ ({", // the export every consumer imports, closed literal types for tsc
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
		"EVENT_TYPE_NO_BUBBLE": clievent.NoBubbleKinds(),
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

// TestContractJS_DeathReasonPrefix pins DEATH_REASON_PREFIX to the cliinfo
// constants the cli package builds suffixed cli_exited reasons from.
func TestContractJS_DeathReasonPrefix(t *testing.T) {
	t.Parallel()
	out, err := Build(filepath.Join("..", "..", "internal", "server", "testdata", "routes.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "  DEATH_REASON_PREFIX: { CODE: '" + cliinfo.DeathReasonCLIExitedCodePrefix +
		"', SIGNAL: '" + cliinfo.DeathReasonCLIExitedSignalPrefix + "' },\n"
	if !strings.Contains(out, want) {
		t.Errorf("contract.js lacks %q", want)
	}
}

// TestContractJS_StartupFailureClass pins STARTUP_FAILURE_CLASS to the wire
// names snapshots carry in startup_failure.class.
func TestContractJS_StartupFailureClass(t *testing.T) {
	t.Parallel()
	out, err := Build(filepath.Join("..", "..", "internal", "server", "testdata", "routes.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "    STARTUP_FAILURE_CLASS: ['" + strings.Join(clierr.AllExitClassWires(), "', '") + "'],\n"
	if !strings.Contains(out, want) {
		t.Errorf("contract.js lacks %q", want)
	}
}

func wireSchemas() (ws, rest string) {
	root := filepath.Join("..", "..")
	return filepath.Join(root, "internal", "wsproto", "wsproto.schema.json"),
		filepath.Join(root, "internal", "dashboard", "session", "testdata", "rest.schema.json")
}

// TestWireDTS_Current byte-compares wire.d.ts against a rebuild from the two
// committed schemas, which their own tests keep current.
func TestWireDTS_Current(t *testing.T) {
	t.Parallel()
	want, err := BuildWireDTS(wireSchemas())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", "internal", "server", "static", "wire.d.ts"))
	if err != nil {
		t.Fatalf("read wire.d.ts (run `go run ./tools/gen-contract`): %v", err)
	}
	if string(got) != want {
		t.Error("wire.d.ts is stale — run `go run ./tools/gen-contract` and commit the result")
	}
}

// TestWireDTS_Anchors pins the shapes tsc leans on: a WsFrames entry per
// frame type with its literal discriminator, EventEntry.type as the closed
// kind union, and omitempty as the only source of `?`.
func TestWireDTS_Anchors(t *testing.T) {
	t.Parallel()
	out, err := BuildWireDTS(wireSchemas())
	if err != nil {
		t.Fatal(err)
	}
	for typ := range wsproto.Frames {
		for _, anchor := range []string{
			"    " + string(typ) + ": WsFrame_" + string(typ) + ";\n",
			"  interface WsFrame_" + string(typ) + " {\n    type: '" + string(typ) + "';\n",
		} {
			if !strings.Contains(out, anchor) {
				t.Errorf("wire.d.ts lacks %q", anchor)
			}
		}
	}
	kinds := "    type: '" + strings.Join(clievent.AllKinds(), "' | '") + "';\n"
	for _, anchor := range []string{
		"declare global {\n",
		"  interface EventEntry {\n",
		kinds,
		"    time: number;\n",                // EventEntry.time has no omitempty
		"    tool_call?: ToolCall;\n",        // omitempty, $ref by short name
		"    sessions: SessionSnapshot[];\n", // REST response, array of a def
		"    sessions: RestResponse_sessions;\n",
	} {
		if !strings.Contains(out, anchor) {
			t.Errorf("wire.d.ts lacks %q", anchor)
		}
	}
}

// TestWireDTS_Rejects covers the schema shapes the generator refuses rather
// than render as a silently wrong type.
func TestWireDTS_Rejects(t *testing.T) {
	t.Parallel()
	frame := `{"properties":{"type":{"type":"string"}},"required":["type"]}`
	for name, tc := range map[string]struct{ ws, rest, want string }{
		"unknown $ref": {
			ws:   `{"types":["a"],"frames":{"a":{"properties":{"type":{"type":"string"},"x":{"type":"object","$ref":"p.Missing"}},"required":["type"]}},"defs":{}}`,
			rest: `{"responses":{},"defs":{}}`,
			want: "$ref p.Missing names no def",
		},
		"short-name collision": {
			ws:   `{"types":["a"],"frames":{"a":` + frame + `},"defs":{"p.Same":{"properties":{},"required":[]}}}`,
			rest: `{"responses":{},"defs":{"q.Same":{"properties":{},"required":[]}}}`,
			want: "share the short name Same",
		},
		"def differs between schemas": {
			ws:   `{"types":["a"],"frames":{"a":` + frame + `},"defs":{"p.D":{"properties":{},"required":[]}}}`,
			rest: `{"responses":{},"defs":{"p.D":{"properties":{"x":{"type":"string"}},"required":[]}}}`,
			want: "def p.D differs",
		},
		"frame without a required type": {
			ws:   `{"types":["a"],"frames":{"a":{"properties":{"type":{"type":"string"}},"required":[]}},"defs":{}}`,
			rest: `{"responses":{},"defs":{}}`,
			want: "no required type key",
		},
		"types list and frames disagree": {
			ws:   `{"types":["a","b"],"frames":{"a":` + frame + `},"defs":{}}`,
			rest: `{"responses":{},"defs":{}}`,
			want: "do not match frames",
		},
		"unmodelled schema keyword": {
			ws:   `{"types":["a"],"frames":{"a":{"properties":{"type":{"type":"string"},"n":{"type":"string","nullable":true}},"required":["type"]}},"defs":{}}`,
			rest: `{"responses":{},"defs":{}}`,
			want: `unknown field "nullable"`,
		},
		"unmodelled object keyword": {
			ws:   `{"types":["a"],"frames":{"a":` + frame + `},"defs":{}}`,
			rest: `{"responses":{},"defs":{"p.D":{"properties":{},"required":[],"additionalProperties":false}}}`,
			want: `unknown field "additionalProperties"`,
		},
		"unmapped schema type": {
			ws:   `{"types":["a"],"frames":{"a":{"properties":{"type":{"type":"string"},"c":{"type":"complex128"}},"required":["type"]}},"defs":{}}`,
			rest: `{"responses":{},"defs":{}}`,
			want: `unmapped schema type "complex128"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ws, rest := filepath.Join(dir, "ws.json"), filepath.Join(dir, "rest.json")
			if err := os.WriteFile(ws, []byte(tc.ws), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rest, []byte(tc.rest), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := BuildWireDTS(ws, rest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildWireDTS error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
