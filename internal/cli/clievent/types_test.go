package clievent_test

import (
	"encoding/json"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestEventEntry_AliasIdentity pins the type-alias contract introduced
// by R217-ARCH-3 (#626): cli.EventEntry must be the identical type to
// clievent.EventEntry, not a separately-named clone. A type alias makes
// both references exchangeable in any context (interface satisfaction,
// JSON shape, struct initialization). If a future commit accidentally
// re-introduces a parallel `type EventEntry struct {...}` in cli, this
// test fails to compile because cli.EventEntry would no longer be
// assignable to a clievent.EventEntry variable without conversion.
func TestEventEntry_AliasIdentity(t *testing.T) {
	// Both directions must compile-time succeed under a true alias.
	var fromLeaf clievent.EventEntry = cli.EventEntry{UUID: "abc"}
	var fromCli cli.EventEntry = clievent.EventEntry{UUID: "abc"}
	if fromLeaf.UUID != fromCli.UUID {
		t.Fatalf("alias roundtrip lost UUID: leaf=%q cli=%q", fromLeaf.UUID, fromCli.UUID)
	}
}

// TestEventEntry_JSONShape pins the wire shape against persisted records.
// Any field rename / json tag drift would silently break replay of
// existing <dataDir>/sessions/*.jsonl files; spot-check the most-load-
// bearing fields here so the cron CI catches it.
func TestEventEntry_JSONShape(t *testing.T) {
	e := clievent.EventEntry{
		UUID:    "u1",
		Time:    1700000000000,
		Type:    "user",
		Summary: "hello",
		AskQuestion: &clievent.AskQuestion{
			ToolUseID: "tu1",
			Items: []clievent.AskQuestionItem{{
				Question: "q?",
				Options:  []clievent.AskQuestionOpt{{Label: "yes"}},
			}},
		},
		ToolCall: &clievent.ToolCall{ID: "tc1", Status: "completed"},
	}
	buf, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantSubs := []string{
		`"uuid":"u1"`,
		`"time":1700000000000`,
		`"type":"user"`,
		`"summary":"hello"`,
		`"ask_question":`,
		`"tool_use_id":"tu1"`,
		`"tool_call":`,
		`"id":"tc1"`,
		`"status":"completed"`,
	}
	got := string(buf)
	for _, s := range wantSubs {
		if !contains(got, s) {
			t.Errorf("JSON missing %q\nfull: %s", s, got)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	// tiny helper avoids importing strings just for one check; behaviour
	// matches strings.Index for our short fixtures.
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestEventEntry_AliasContract_R247_ARCH_16 anchors R247-ARCH-16 (#659):
// the proposed `eventcore` types-only sub-package landed as
// `internal/cli/clievent` (cluster anchor R246-ARCH-13). cli.EventEntry is
// now a type alias to clievent.EventEntry so internal/history/* — which
// reads/writes EventEntry off persisted JSONL and would otherwise form a
// diamond import — can bind to the leaf without dragging in the rest of
// the cli surface.
//
// This test pins three invariants that together cover the R247-ARCH-16
// regression surface so the issue can close:
//
//  1. Both directions of the alias compile (already covered by
//     TestEventEntry_AliasIdentity for #626 — duplicated as a smoke check
//     here so the #659 anchor isn't fragile to that test being moved).
//  2. The leaf carries every field the cli ring-buffer depends on, so a
//     future refactor that moves a field back into cli (re-introducing
//     a behavioural type) would break the construction below.
//  3. The leaf MUST stay constructible from outside the cli package
//     without conversion — proven by the `cli.EventEntry{...}` literal
//     compiling in this test that imports both. Replacing the alias
//     with a separate struct would break the literal.
//
// If you find yourself adding `behaviour` (methods, tagged-union state)
// to clievent.EventEntry and have to weaken any of these checks, that's
// the signal the leaf has outgrown its types-only charter — split a new
// `eventcore` (or similar) leaf rather than relaxing the contract.
func TestEventEntry_AliasContract_R247_ARCH_16(t *testing.T) {
	// (1) bidirectional alias — same as #626 anchor; restated here so
	// #659 stays self-contained.
	var fromLeaf clievent.EventEntry = cli.EventEntry{UUID: "anchor-659"}
	var fromCli cli.EventEntry = clievent.EventEntry{UUID: "anchor-659"}
	if fromLeaf.UUID != "anchor-659" || fromCli.UUID != "anchor-659" {
		t.Fatalf("alias direction lost field: leaf=%+v cli=%+v", fromLeaf, fromCli)
	}

	// (2) the load-bearing fields used by history/source.go consumers must
	// live on the leaf (NOT on a cli-side wrapper). Construct with each
	// field set so a future refactor that drops one off the leaf surfaces
	// here as a build break.
	e := clievent.EventEntry{
		UUID:            "u1",
		Time:            1700000000000,
		Type:            "task_start",
		Summary:         "s",
		ToolUseID:       "tu1",
		TaskID:          "task-x",
		InternalAgentID: "ia1",
		JSONLPath:       "/tmp/agent.jsonl",
		FirstPromptID:   "fp1",
	}
	if e.TaskID != "task-x" || e.InternalAgentID != "ia1" {
		t.Fatalf("leaf field roundtrip lost: %+v", e)
	}

	// (3) no-conversion assignment in both directions confirms the
	// compiler treats the names as a single type. A `type EventEntry
	// = clievent.EventEntry` alias makes this trivial; a `type
	// EventEntry struct { ... }` copy would force an explicit
	// conversion and the line below would not compile.
	var same cli.EventEntry = e
	if same.UUID != e.UUID {
		t.Fatalf("no-conversion assignment dropped UUID: %q vs %q", same.UUID, e.UUID)
	}
}
