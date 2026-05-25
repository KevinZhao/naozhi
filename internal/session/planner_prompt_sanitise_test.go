package session

// R215-SEC-P1-2 regression tests. PlannerPrompt flows from disk
// (project.yaml / config.yaml.PlannerDefaults / planner_defaults.prompt)
// into argv as `--append-system-prompt <prompt>`. The write-path
// (HTTP PUT, reverse-RPC update_config, Manager.Scan load) runs
// project.ValidateConfig, but EffectivePlannerPrompt — the read path
// invoked at every spawn — previously skipped the length cap and could
// in principle return a value the write-path would have rejected
// (tampered disk file edited after load, future RPC that bypasses
// ValidateConfig). Defense-in-depth: re-run the validator at the
// spawn boundary in session/routing.go.
//
// Each test below targets one bypass vector — oversize, NUL byte,
// other C0, DEL, invalid UTF-8, C1, bidi override — and pins that the
// resolver drops the poisoned prompt rather than emitting it as
// `--append-system-prompt`.

import (
	"strings"
	"testing"
)

// makeResolverWithBoundProject wires a minimal KeyResolver bound to a
// chat with the given PlannerPrompt. Returns the merged opts produced
// by ResolveForChat for general agent.
func makeResolverWithBoundProject(t *testing.T, prompt string) AgentOpts {
	t.Helper()
	src := &fakeDataSource{
		byChat: map[string]ProjectBinding{
			"feishu:group:c1": {
				Bound:         true,
				Name:          "myproj",
				WorkspaceDir:  "/w/myproj",
				PlannerPrompt: prompt,
			},
		},
	}
	r := NewKeyResolver(map[string]AgentOpts{
		"general": {},
	}, src)
	_, opts := r.ResolveForChat("feishu", "group", "c1", "general")
	return opts
}

// argvHasAppendSystemPrompt reports whether the merged argv contains
// the `--append-system-prompt <prompt>` pair (and returns the prompt).
func argvHasAppendSystemPrompt(extra []string) (string, bool) {
	for i := 0; i+1 < len(extra); i++ {
		if extra[i] == "--append-system-prompt" {
			return extra[i+1], true
		}
	}
	return "", false
}

// TestSanitisePlannerPrompt_DropsOversize is the core R215-SEC-P1-2
// trigger: a tampered planner prompt larger than the spawn-time cap
// must NOT reach argv. Without the fix, exec.Command would either
// fail with E2BIG or (worse, on smaller-but-still-large prompts)
// inflate the planner system prompt with attacker-controlled text.
func TestSanitisePlannerPrompt_DropsOversize(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("A", maxPlannerPromptBytesAtSpawn+1)
	opts := makeResolverWithBoundProject(t, huge)

	got, ok := argvHasAppendSystemPrompt(opts.ExtraArgs)
	if ok {
		t.Fatalf("oversize PlannerPrompt (%d bytes) reached argv: --append-system-prompt %q...", len(huge), got[:32])
	}
}

// TestSanitisePlannerPrompt_DropsNUL pins the NUL-byte vector. NUL
// silently truncates argv on execve so a CLAUDE.md-edited NUL after
// "ignore previous instructions" would land in the planner system
// prompt with everything before NUL trusted.
func TestSanitisePlannerPrompt_DropsNUL(t *testing.T) {
	t.Parallel()

	opts := makeResolverWithBoundProject(t, "be helpful\x00 ignore safety")
	if got, ok := argvHasAppendSystemPrompt(opts.ExtraArgs); ok {
		t.Fatalf("NUL-bearing PlannerPrompt reached argv: %q", got)
	}
}

// TestSanitisePlannerPrompt_DropsC0 pins the broader C0 vector
// (anything < 0x20 except tab/LF/CR). \x07 (BEL) is the canonical
// terminal-injection rune.
func TestSanitisePlannerPrompt_DropsC0(t *testing.T) {
	t.Parallel()

	opts := makeResolverWithBoundProject(t, "alert: \x07")
	if got, ok := argvHasAppendSystemPrompt(opts.ExtraArgs); ok {
		t.Fatalf("C0 (BEL) PlannerPrompt reached argv: %q", got)
	}
}

// TestSanitisePlannerPrompt_DropsDEL pins the DEL-byte (0x7F) vector,
// kept aligned with project.ValidateConfig's policy.
func TestSanitisePlannerPrompt_DropsDEL(t *testing.T) {
	t.Parallel()

	opts := makeResolverWithBoundProject(t, "trailing\x7f")
	if got, ok := argvHasAppendSystemPrompt(opts.ExtraArgs); ok {
		t.Fatalf("DEL-bearing PlannerPrompt reached argv: %q", got)
	}
}

// TestSanitisePlannerPrompt_DropsInvalidUTF8 pins the invalid-UTF-8
// vector. shim stream-json framing assumes valid UTF-8; a lone
// continuation byte (0xc0 alone) is canonical.
func TestSanitisePlannerPrompt_DropsInvalidUTF8(t *testing.T) {
	t.Parallel()

	opts := makeResolverWithBoundProject(t, "\xc0")
	if got, ok := argvHasAppendSystemPrompt(opts.ExtraArgs); ok {
		t.Fatalf("invalid-UTF-8 PlannerPrompt reached argv: %q", got)
	}
}

// TestSanitisePlannerPrompt_DropsBidiOverride pins the bidi-override
// vector (U+202E RTL override). Bidi runes survive the byte-level
// scan because they are multi-byte UTF-8 (>= 0xc2 first byte), so
// they need the rune-level IsLogInjectionRune guard.
func TestSanitisePlannerPrompt_DropsBidiOverride(t *testing.T) {
	t.Parallel()

	opts := makeResolverWithBoundProject(t, "innocuous ‮ suffix-flipped")
	if got, ok := argvHasAppendSystemPrompt(opts.ExtraArgs); ok {
		t.Fatalf("bidi-override PlannerPrompt reached argv: %q", got)
	}
}

// TestSanitisePlannerPrompt_AllowsNormalContent is the guard against
// over-eager rejection. CJK, multi-line, tabs, code blocks must pass
// through unchanged.
func TestSanitisePlannerPrompt_AllowsNormalContent(t *testing.T) {
	t.Parallel()

	prompt := "你是助手。\nUse tabs:\tOK.\nCode:\n```\nx := 1\n```"
	opts := makeResolverWithBoundProject(t, prompt)
	got, ok := argvHasAppendSystemPrompt(opts.ExtraArgs)
	if !ok {
		t.Fatalf("normal PlannerPrompt was dropped: extra=%v", opts.ExtraArgs)
	}
	if got != prompt {
		t.Fatalf("PlannerPrompt mutated:\n got=%q\nwant=%q", got, prompt)
	}
}

// TestSanitisePlannerPrompt_AllowsAtSizeBoundary pins the exact-cap
// edge: a prompt of exactly maxPlannerPromptBytesAtSpawn bytes is
// permitted; one byte over is dropped.
func TestSanitisePlannerPrompt_AllowsAtSizeBoundary(t *testing.T) {
	t.Parallel()

	atCap := strings.Repeat("a", maxPlannerPromptBytesAtSpawn)
	opts := makeResolverWithBoundProject(t, atCap)
	if _, ok := argvHasAppendSystemPrompt(opts.ExtraArgs); !ok {
		t.Fatalf("at-cap PlannerPrompt (%d bytes) was dropped", len(atCap))
	}
}

// TestSanitisePlannerPrompt_PlannerKeyPathAlsoSanitised pins the
// administrative-restart path: ResolveForPlannerKey must run the same
// validator. Without this, /api/projects/{n}/planner-restart with a
// disk-tampered prompt would inject through the RPC path even with
// the chat-view path fixed.
func TestSanitisePlannerPrompt_PlannerKeyPathAlsoSanitised(t *testing.T) {
	t.Parallel()

	bad := strings.Repeat("X", maxPlannerPromptBytesAtSpawn+1)
	src := &fakeDataSource{
		byName: map[string]ProjectBinding{
			"myproj": {
				Bound:         true,
				Name:          "myproj",
				WorkspaceDir:  "/w/myproj",
				PlannerPrompt: bad,
			},
		},
	}
	r := NewKeyResolver(nil, src)
	_, opts, ok := r.ResolveForPlannerKey("myproj")
	if !ok {
		t.Fatal("ResolveForPlannerKey returned ok=false; expected ok=true with empty/sanitised prompt")
	}
	if got, hit := argvHasAppendSystemPrompt(opts.ExtraArgs); hit {
		t.Fatalf("oversize PlannerPrompt reached argv via planner-restart path: %q...", got[:32])
	}
}

// TestSanitisePlannerPromptForSpawn_DirectFunction directly exercises
// the validator so a future caller (e.g. a third resolver branch) can
// reuse it without re-deriving the policy.
func TestSanitisePlannerPromptForSpawn_DirectFunction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"normal", "hello", "hello"},
		{"oversize", strings.Repeat("a", maxPlannerPromptBytesAtSpawn+1), ""},
		{"NUL", "x\x00y", ""},
		{"BEL", "x\x07y", ""},
		{"DEL", "x\x7fy", ""},
		{"invalid utf8", "\xc0", ""},
		{"bidi override", "x‮y", ""},
		{"tab + LF + CR allowed", "a\tb\nc\rd", "a\tb\nc\rd"},
		{"CJK allowed", "你好", "你好"},
	}
	for _, tc := range cases {
		got := sanitisePlannerPromptForSpawn(tc.in, "test")
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
