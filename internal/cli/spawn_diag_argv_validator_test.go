package cli

import (
	"strings"
	"testing"
)

// The argv validator drops a dedicated SpawnOptions field whose value it cannot
// render safely — a malformed resume id, a relative debug/mcp path, an oversized
// system prompt. Those drops used to be bare slog.Warns inside
// ClaudeProtocol.BuildArgs (two of them) or entirely silent (the other two), so
// an operator who set cli.debug_file to a relative path got no CLI debug
// capture and no signal anywhere. SpawnDiagsFor now derives them from the same predicates the builder
// uses, which is what puts them in metrics, /api/sessions and config check.

// diagFor returns the diag for key, or the zero value.
func diagFor(diags []SpawnDiag, key string) SpawnDiag {
	for _, d := range diags {
		if d.Key == key {
			return d
		}
	}
	return SpawnDiag{}
}

func TestSpawnDiagsFor_ArgvValidatorDrops(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		opts       SpawnOptions
		wantKey    string
		wantInText string
	}{
		{
			// Outside [A-Za-z0-9-]: a space and a '!'. A leading hyphen is legal
			// in the pattern, which is why the strict UUID gate lives upstream.
			name:       "malformed resume id",
			opts:       SpawnOptions{ResumeID: "bad id!"},
			wantKey:    "--resume",
			wantInText: "fresh session",
		},
		{
			name:       "relative debug file",
			opts:       SpawnOptions{DebugFile: "debug.log"},
			wantKey:    "--debug-file",
			wantInText: "absolute",
		},
		{
			name:       "debug file starting with a dash",
			opts:       SpawnOptions{DebugFile: "-/tmp/x.log"},
			wantKey:    "--debug-file",
			wantInText: "absolute",
		},
		{
			name:       "relative mcp config",
			opts:       SpawnOptions{MCPConfigFile: "mcp.json"},
			wantKey:    "--mcp-config",
			wantInText: "absolute",
		},
		{
			name:       "system prompt with a leading dash",
			opts:       SpawnOptions{AppendSystemPrompt: "-rm -rf"},
			wantKey:    "--append-system-prompt",
			wantInText: "rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := diagFor(SpawnDiagsFor(tc.opts, Caps{}), tc.wantKey)
			if d.Key == "" {
				t.Fatalf("no diag for %s; the drop would be invisible", tc.wantKey)
			}
			if d.Layer != "argv-validator" || d.Action != "dropped" {
				t.Errorf("diag = layer %q action %q, want argv-validator/dropped", d.Layer, d.Action)
			}
			if !strings.Contains(d.Reason, tc.wantInText) {
				t.Errorf("reason %q must mention %q", d.Reason, tc.wantInText)
			}
		})
	}
}

// TestSpawnDiagsFor_ArgvValidatorAgreesWithTheBuilder is the point of deriving
// the diags from the builder's own predicates: for each field, "a diag exists"
// and "the flag is missing from argv" must always agree. A future edit that
// loosens one side without the other fails here.
func TestSpawnDiagsFor_ArgvValidatorAgreesWithTheBuilder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts SpawnOptions
		flag string
	}{
		{"good resume id", SpawnOptions{ResumeID: "abc-123"}, "--resume"},
		{"bad resume id", SpawnOptions{ResumeID: "bad id!"}, "--resume"},
		{"absolute debug file", SpawnOptions{DebugFile: "/tmp/d.log"}, "--debug-file"},
		{"relative debug file", SpawnOptions{DebugFile: "d.log"}, "--debug-file"},
		{"absolute mcp config", SpawnOptions{MCPConfigFile: "/tmp/m.json"}, "--mcp-config"},
		{"relative mcp config", SpawnOptions{MCPConfigFile: "m.json"}, "--mcp-config"},
		{"clean system prompt", SpawnOptions{AppendSystemPrompt: "be terse"}, "--append-system-prompt"},
		{"dash system prompt", SpawnOptions{AppendSystemPrompt: "-x"}, "--append-system-prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			argv := (&ClaudeProtocol{}).BuildArgs(tc.opts)
			inArgv := false
			for _, a := range argv {
				if a == tc.flag {
					inArgv = true
					break
				}
			}
			reported := diagFor(SpawnDiagsFor(tc.opts, Caps{}), tc.flag).Key != ""
			if inArgv == reported {
				t.Errorf("%s: rendered in argv=%v, reported as dropped=%v — the builder and the diag disagree",
					tc.flag, inArgv, reported)
			}
		})
	}
}

// A rejected value must never be echoed whole: a ResumeID is attacker-influenced
// and a system prompt can be huge, so a reason carrying either would turn the
// diag into a log-flooding amplifier.
func TestSpawnDiagsFor_ArgvValidatorNeverEchoesTheValue(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("A", 200)
	diags := SpawnDiagsFor(SpawnOptions{
		ResumeID:           "-" + long,
		AppendSystemPrompt: "-" + long,
	}, Caps{})
	if len(diags) == 0 {
		t.Fatal("expected drops for both fields")
	}
	for _, d := range diags {
		if strings.Contains(d.Reason, long) {
			t.Errorf("reason for %s echoes the whole rejected value: %q", d.Key, d.Reason)
		}
		if len(d.Reason) > 200 {
			t.Errorf("reason for %s is %d bytes; keep it a sentence", d.Key, len(d.Reason))
		}
	}
	// The resume prefix is capped at 16 bytes, so a 200-byte id cannot flood.
	if r := diagFor(diags, "--resume").Reason; strings.Count(r, "A") > 16 {
		t.Errorf("resume reason echoed %d bytes of the id, want ≤16: %q", strings.Count(r, "A"), r)
	}
}
