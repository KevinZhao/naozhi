package server

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestRedactEntrySecrets pins R20260604-SEC-10: the WS history frame path must
// scrub credential token shapes from Summary/Detail before fan-out, without
// mutating EventLog's shared ring buffer (copy-on-write), and without
// allocating on the clean path.
func TestRedactEntrySecrets(t *testing.T) {
	const secret = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA"

	t.Run("redacts summary and detail", func(t *testing.T) {
		in := []cli.EventEntry{
			{Type: "text", Summary: "key is " + secret, Detail: "also " + secret + " here"},
		}
		out := redactEntrySecrets(in)
		if got := out[0].Summary; got == in[0].Summary {
			t.Fatalf("summary not redacted: %q", got)
		}
		if strings.Contains(out[0].Summary, secret) {
			t.Errorf("secret survived in summary: %q", out[0].Summary)
		}
		if strings.Contains(out[0].Detail, secret) {
			t.Errorf("secret survived in detail: %q", out[0].Detail)
		}
	})

	t.Run("does not mutate shared input buffer", func(t *testing.T) {
		in := []cli.EventEntry{
			{Type: "text", Summary: "leak " + secret},
		}
		origSummary := in[0].Summary
		out := redactEntrySecrets(in)
		if in[0].Summary != origSummary {
			t.Errorf("input entry mutated in place: %q", in[0].Summary)
		}
		if &out[0] == &in[0] {
			t.Errorf("dirty path returned aliased slice; copy-on-write broken")
		}
	})

	t.Run("clean input is aliased not copied", func(t *testing.T) {
		in := []cli.EventEntry{
			{Type: "text", Summary: "nothing sensitive here", Detail: "plain detail"},
			{Type: "result", Summary: "done"},
		}
		out := redactEntrySecrets(in)
		if len(in) == 0 || len(out) == 0 {
			t.Fatal("unexpected empty slices")
		}
		if &out[0] != &in[0] {
			t.Errorf("clean path allocated a new slice; want aliased input")
		}
	})

	t.Run("empty slice", func(t *testing.T) {
		out := redactEntrySecrets(nil)
		if len(out) != 0 {
			t.Errorf("nil input produced %d entries", len(out))
		}
	})

	t.Run("only later entry dirty still preserves earlier entries", func(t *testing.T) {
		in := []cli.EventEntry{
			{Type: "text", Summary: "clean one"},
			{Type: "text", Summary: "dirty " + secret},
		}
		out := redactEntrySecrets(in)
		if out[0].Summary != "clean one" {
			t.Errorf("clean entry corrupted: %q", out[0].Summary)
		}
		if strings.Contains(out[1].Summary, secret) {
			t.Errorf("secret survived: %q", out[1].Summary)
		}
		// Earlier (unchanged) entry must equal the original value, and the
		// shared input slot for it must be untouched.
		if in[0].Summary != "clean one" || in[1].Summary == out[1].Summary {
			t.Errorf("input buffer mutated: in=%+v", in)
		}
	})
}
