package session

import (
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestSanitizeQuote(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantContains string
		wantAbsent   []string
		wantTrunc    bool
	}{
		{
			name:         "plain text preserved",
			in:           "hello world",
			wantContains: "hello world",
		},
		{
			name:         "newlines and tabs kept",
			in:           "line1\nline2\tindent",
			wantContains: "line1\nline2\tindent",
		},
		{
			name:         "control chars stripped",
			in:           "good\x00bad\x07still",
			wantContains: "goodbadstill",
			wantAbsent:   []string{"\x00", "\x07"},
		},
		{
			name:         "bidi overrides stripped",
			in:           "clean\u202etext",
			wantContains: "cleantext",
			wantAbsent:   []string{"\u202e"},
		},
		{
			name:         "zero-width removed",
			in:           "a\u200bb\u200cc",
			wantContains: "abc",
			wantAbsent:   []string{"\u200b", "\u200c"},
		},
		{
			name:         "BOM removed",
			in:           "\uFEFFstart",
			wantContains: "start",
			wantAbsent:   []string{"\uFEFF"},
		},
		{
			name:         "C1 control removed",
			in:           "a\u0085b",
			wantContains: "ab",
			wantAbsent:   []string{"\u0085"},
		},
		{
			name:      "truncate oversize",
			in:        strings.Repeat("x", MaxScratchQuoteBytes+500),
			wantTrunc: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, trunc := SanitizeQuote(tc.in)
			if trunc != tc.wantTrunc {
				t.Errorf("truncated=%v want %v", trunc, tc.wantTrunc)
			}
			if tc.wantContains != "" && !strings.Contains(got, tc.wantContains) {
				t.Errorf("missing %q in %q", tc.wantContains, got)
			}
			for _, s := range tc.wantAbsent {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q in %q", s, got)
				}
			}
			if tc.wantTrunc && len(got) > MaxScratchQuoteBytes {
				t.Errorf("truncated result length %d exceeds cap %d", len(got), MaxScratchQuoteBytes)
			}
		})
	}
}

func TestSanitizeQuote_UTF8BoundaryAfterTruncate(t *testing.T) {
	// Build input where the cut point would fall mid-rune unless the function
	// walks back to a RuneStart. Use a 3-byte CJK rune straddling the cap.
	rune3 := "語" // 3 bytes
	prefix := strings.Repeat("a", MaxScratchQuoteBytes-2)
	in := prefix + rune3 + "tail"
	got, trunc := SanitizeQuote(in)
	if !trunc {
		t.Fatal("expected truncation")
	}
	if len(got) > MaxScratchQuoteBytes {
		t.Fatalf("truncated length %d exceeds cap", len(got))
	}
	// The split rune should be dropped entirely, not half-emitted.
	if strings.HasSuffix(got, "\xe8") || strings.HasSuffix(got, "\xe8\xaa") {
		t.Fatalf("truncation left partial UTF-8 bytes: %q", got[len(got)-4:])
	}
}

func TestSanitizeQuote_EmptyAfterCleaning(t *testing.T) {
	got, trunc := SanitizeQuote("\x00\x01\x02")
	if got != "" || trunc {
		t.Errorf("got (%q, %v); want (\"\", false)", got, trunc)
	}
}

func TestScratchPool_OpenRegistersByKey(t *testing.T) {
	p := NewScratchPool(nil, 5, time.Minute)
	sc, err := p.Open(OpenOptions{
		SourceKey: "feishu:group:chat1:general",
		AgentID:   "general",
		Quote:     "what does this mean?",
		BaseOpts:  AgentOpts{Model: "opus"},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !strings.HasPrefix(sc.Key, ScratchKeyPrefix) {
		t.Errorf("key %q missing prefix", sc.Key)
	}
	opts, ok := p.OptsForKey(sc.Key)
	if !ok {
		t.Fatal("OptsForKey miss")
	}
	if opts.Model != "opus" {
		t.Errorf("model inheritance broken: %q", opts.Model)
	}
	if len(opts.ExtraArgs) < 2 {
		t.Fatalf("ExtraArgs missing append-system-prompt: %v", opts.ExtraArgs)
	}
	last := opts.ExtraArgs[len(opts.ExtraArgs)-2]
	if last != "--append-system-prompt" {
		t.Errorf("tail arg not append-system-prompt: %q", last)
	}
	if !strings.Contains(opts.ExtraArgs[len(opts.ExtraArgs)-1], "what does this mean?") {
		t.Errorf("prompt body missing quote")
	}
	if opts.Exempt {
		t.Error("scratch opts should not be exempt")
	}
}

func TestScratchPool_EmptyQuoteRejected(t *testing.T) {
	p := NewScratchPool(nil, 5, time.Minute)
	_, err := p.Open(OpenOptions{
		SourceKey: "feishu:group:chat1:general",
		Quote:     "\x00\x01",
	})
	if err == nil {
		t.Fatal("expected ErrQuoteEmpty")
	}
}

func TestScratchPool_CapEnforced(t *testing.T) {
	p := NewScratchPool(nil, 2, time.Minute)
	for i := 0; i < 2; i++ {
		if _, err := p.Open(OpenOptions{SourceKey: "k:direct:u:general", Quote: "x"}); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	_, err := p.Open(OpenOptions{SourceKey: "k:direct:u:general", Quote: "x"})
	if err == nil {
		t.Fatal("expected ErrScratchPoolFull")
	}
}

func TestScratchPool_GetAndDetach(t *testing.T) {
	p := NewScratchPool(nil, 5, time.Minute)
	sc, err := p.Open(OpenOptions{SourceKey: "k:direct:u:general", Quote: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Get(sc.ID) == nil {
		t.Fatal("Get miss")
	}
	if _, err := p.Detach(sc.ID); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if p.Get(sc.ID) != nil {
		t.Error("Get should return nil after Detach")
	}
	if _, ok := p.OptsForKey(sc.Key); ok {
		t.Error("OptsForKey should miss after Detach")
	}
}

func TestScratchPool_CloseUnknown(t *testing.T) {
	p := NewScratchPool(nil, 5, time.Minute)
	if err := p.Close("00000000000000000000000000000000"); err != ErrScratchNotFound {
		t.Errorf("got %v, want ErrScratchNotFound", err)
	}
}

func TestIsScratchKey(t *testing.T) {
	if !IsScratchKey("scratch:abc:general:general") {
		t.Error("scratch-prefixed key not recognised")
	}
	if IsScratchKey("feishu:group:chat:general") {
		t.Error("normal key falsely recognised")
	}
	if IsScratchKey("") {
		t.Error("empty key falsely recognised")
	}
}

func TestScratchPool_SweepEvictsIdle(t *testing.T) {
	p := NewScratchPool(nil, 5, 50*time.Millisecond)
	sc, err := p.Open(OpenOptions{SourceKey: "k:direct:u:general", Quote: "x"})
	if err != nil {
		t.Fatal(err)
	}
	sc.lastUsed.Store(time.Now().Add(-10 * time.Second).UnixNano())
	p.sweep(time.Now())
	if p.Get(sc.ID) != nil {
		t.Error("sweep should have evicted idle scratch")
	}
	if _, ok := p.OptsForKey(sc.Key); ok {
		t.Error("byKey should have been cleared by sweep")
	}
}

func TestScratchPool_ExtraArgsNotAliased(t *testing.T) {
	original := []string{"--model", "opus"}
	p := NewScratchPool(nil, 5, time.Minute)
	_, err := p.Open(OpenOptions{
		SourceKey: "k:direct:u:general",
		Quote:     "q1",
		BaseOpts:  AgentOpts{ExtraArgs: original},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 2 {
		t.Fatalf("caller's slice mutated: %v", original)
	}
}

// TestScratchPool_AgentIDWithColonSanitized verifies that a malicious or
// misconfigured agentID containing a colon does not corrupt the 4-segment
// key invariant. sanitizeKeyComponent replaces `:` with `_`, so the key
// stays parseable by SplitN(..., 4) in the promote handler.
func TestScratchPool_AgentIDWithColonSanitized(t *testing.T) {
	p := NewScratchPool(nil, 5, time.Minute)
	sc, err := p.Open(OpenOptions{
		SourceKey: "k:direct:u:general",
		AgentID:   "evil:agent",
		Quote:     "hi",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Key must still split into exactly 4 segments; the colon in agentID
	// should have been replaced, not preserved.
	parts := strings.SplitN(sc.Key, ":", 5)
	if len(parts) < 4 || len(parts) == 5 {
		t.Fatalf("key %q does not split into 4 segments (got %d)", sc.Key, len(parts))
	}
	if strings.Contains(parts[3], ":") {
		t.Errorf("agent segment leaked a colon: %q", parts[3])
	}
	// Validate the shape so downstream ValidateSessionKey would accept it.
	if err := ValidateSessionKey(sc.Key); err != nil {
		t.Errorf("promoted key failed ValidateSessionKey: %v", err)
	}
}

// TestRenderContextTurns_FiltersNoise verifies that tool_use / thinking /
// todo / init entries are dropped even when they arrive mixed with
// conversational turns, and that only user/text/result survive into the
// rendered block.
func TestRenderContextTurns_FiltersNoise(t *testing.T) {
	before := []cli.EventEntry{
		{Type: "user", Detail: "first question"},
		{Type: "tool_use", Tool: "Read", Summary: "Read file"},
		{Type: "thinking", Summary: "pondering"},
		{Type: "text", Detail: "first answer"},
		{Type: "todo", Summary: "[]"},
	}
	block, turns, trunc := renderContextTurns(before, nil, MaxScratchContextBytes)
	if trunc {
		t.Errorf("did not expect truncation, got trunc=true block=%q", block)
	}
	if turns != 2 {
		t.Errorf("turns = %d, want 2 (user+text only)", turns)
	}
	if !strings.Contains(block, "[user] first question") {
		t.Errorf("block missing user line: %q", block)
	}
	if !strings.Contains(block, "[assistant] first answer") {
		t.Errorf("block missing assistant line: %q", block)
	}
	for _, noise := range []string{"tool_use", "thinking", "todo", "Read file", "pondering"} {
		if strings.Contains(block, noise) {
			t.Errorf("block should not carry %q: %q", noise, block)
		}
	}
}

// TestRenderContextTurns_OrderBeforeThenAfter ensures the rendered block
// preserves chronological order: all before entries (oldest→newest)
// followed by all after entries (oldest→newest). The quote itself is not
// part of either slice — the caller excludes it.
func TestRenderContextTurns_OrderBeforeThenAfter(t *testing.T) {
	before := []cli.EventEntry{
		{Type: "user", Detail: "b1"},
		{Type: "text", Detail: "b2"},
	}
	after := []cli.EventEntry{
		{Type: "text", Detail: "a1"},
		{Type: "user", Detail: "a2"},
	}
	block, turns, _ := renderContextTurns(before, after, MaxScratchContextBytes)
	if turns != 4 {
		t.Fatalf("turns = %d, want 4", turns)
	}
	want := []string{"b1", "b2", "a1", "a2"}
	idx := -1
	for _, s := range want {
		pos := strings.Index(block, s)
		if pos < 0 {
			t.Fatalf("%q missing from block: %q", s, block)
		}
		if pos <= idx {
			t.Fatalf("order violation for %q in %q", s, block)
		}
		idx = pos
	}
}

// TestRenderContextTurns_BudgetTruncation builds a set of entries whose
// rendered size exceeds the budget and verifies the returned block fits,
// the truncated flag is set, and entries closest to the quote are the
// ones retained (tail of `before`, head of `after`).
func TestRenderContextTurns_BudgetTruncation(t *testing.T) {
	big := strings.Repeat("x", 1024) // 1 KiB payload per entry
	before := []cli.EventEntry{
		{Type: "user", Detail: "farthest:" + big},
		{Type: "text", Detail: "middle:" + big},
		{Type: "user", Detail: "closest-before:" + big},
	}
	after := []cli.EventEntry{
		{Type: "text", Detail: "closest-after:" + big},
		{Type: "user", Detail: "distant-after:" + big},
	}
	// Budget accommodates ~2 entries of ~1 KiB each.
	block, turns, trunc := renderContextTurns(before, after, 2500)
	if !trunc {
		t.Error("expected truncated=true")
	}
	if turns < 1 || turns >= 5 {
		t.Errorf("turns = %d, want a partial subset", turns)
	}
	if len(block) > 2500 {
		t.Errorf("block size %d exceeds budget", len(block))
	}
	// Because "closest-before" sits at the tail of before, it should be
	// included before "farthest". Likewise "closest-after" must appear
	// before "distant-after" gets considered.
	if strings.Contains(block, "farthest:") && !strings.Contains(block, "closest-before:") {
		t.Error("budget pruned the close turn but kept the far one")
	}
	if strings.Contains(block, "distant-after:") && !strings.Contains(block, "closest-after:") {
		t.Error("after-side ordering violated")
	}
}

// TestRenderContextTurns_EmptyInputs returns empty strings without a
// truncation flag when there is nothing to render.
func TestRenderContextTurns_EmptyInputs(t *testing.T) {
	block, turns, trunc := renderContextTurns(nil, nil, MaxScratchContextBytes)
	if block != "" || turns != 0 || trunc {
		t.Errorf("got (%q, %d, %v); want empty/zero/false", block, turns, trunc)
	}
}

// TestRenderContextTurns_ZeroBudget with candidates available must report
// truncation (context was suppressed) and return an empty block.
func TestRenderContextTurns_ZeroBudget(t *testing.T) {
	before := []cli.EventEntry{{Type: "user", Detail: "q"}}
	block, turns, trunc := renderContextTurns(before, nil, 0)
	if block != "" {
		t.Errorf("block should be empty when budget=0, got %q", block)
	}
	if turns != 0 {
		t.Errorf("turns = %d, want 0", turns)
	}
	if !trunc {
		t.Error("trunc=false but context was suppressed due to zero budget")
	}
}

// TestScratchPool_OpenInjectsContextBlock checks that Open renders the
// surrounding turns into the --append-system-prompt value produced for
// the router, and populates the Scratch metadata.
func TestScratchPool_OpenInjectsContextBlock(t *testing.T) {
	p := NewScratchPool(nil, 5, time.Minute)
	before := []cli.EventEntry{
		{Type: "user", Detail: "what is the circuit breaker?"},
		{Type: "text", Detail: "a guard that trips on error."},
	}
	after := []cli.EventEntry{
		{Type: "user", Detail: "how do I tune it?"},
	}
	sc, err := p.Open(OpenOptions{
		SourceKey:     "feishu:direct:alice:general",
		AgentID:       "general",
		Quote:         "trips on error.",
		ContextBefore: before,
		ContextAfter:  after,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if sc.ContextTurns != 3 {
		t.Errorf("ContextTurns = %d, want 3", sc.ContextTurns)
	}
	if sc.ContextTrunc {
		t.Error("context should not be truncated under full budget")
	}
	prompt := sc.BaseOpts.ExtraArgs[len(sc.BaseOpts.ExtraArgs)-1]
	if !strings.Contains(prompt, "<conversation_context>") {
		t.Errorf("prompt missing <conversation_context> block: %q", prompt)
	}
	if !strings.Contains(prompt, "[user] what is the circuit breaker?") {
		t.Errorf("prompt missing first user turn: %q", prompt)
	}
	if !strings.Contains(prompt, "[assistant] a guard that trips on error.") {
		t.Errorf("prompt missing assistant turn: %q", prompt)
	}
	if !strings.Contains(prompt, "[user] how do I tune it?") {
		t.Errorf("prompt missing after-turn: %q", prompt)
	}
	if !strings.Contains(prompt, "<selected_quote>") {
		t.Errorf("prompt missing <selected_quote> block: %q", prompt)
	}
}

// TestScratchPool_OpenNoContext confirms backward compatibility: when the
// caller provides no context entries the prompt shape remains simple
// (no conversation_context block, quote still present).
func TestScratchPool_OpenNoContext(t *testing.T) {
	p := NewScratchPool(nil, 5, time.Minute)
	sc, err := p.Open(OpenOptions{
		SourceKey: "feishu:direct:alice:general",
		Quote:     "ping",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.ContextTurns != 0 || sc.ContextTrunc {
		t.Errorf("unexpected context metadata: turns=%d trunc=%v", sc.ContextTurns, sc.ContextTrunc)
	}
	prompt := sc.BaseOpts.ExtraArgs[len(sc.BaseOpts.ExtraArgs)-1]
	if strings.Contains(prompt, "<conversation_context>") {
		t.Errorf("empty context should skip the block, got %q", prompt)
	}
	if !strings.Contains(prompt, "<selected_quote>") {
		t.Errorf("prompt missing quote block: %q", prompt)
	}
}

// TestScratchPool_OpenContextSharesBudgetWithQuote verifies the context
// budget shrinks when a large quote consumes most of MaxScratchContextBytes.
func TestScratchPool_OpenContextSharesBudgetWithQuote(t *testing.T) {
	p := NewScratchPool(nil, 5, time.Minute)
	hugeQuote := strings.Repeat("y", MaxScratchQuoteBytes) // fills the quote cap
	big := strings.Repeat("x", 2048)
	before := []cli.EventEntry{
		{Type: "user", Detail: big},
		{Type: "text", Detail: big},
	}
	sc, err := p.Open(OpenOptions{
		SourceKey:     "feishu:direct:alice:general",
		Quote:         hugeQuote,
		ContextBefore: before,
	})
	if err != nil {
		t.Fatal(err)
	}
	prompt := sc.BaseOpts.ExtraArgs[len(sc.BaseOpts.ExtraArgs)-1]
	if len(prompt) > MaxScratchContextBytes+512 { // +512 for wrapper + templates
		t.Errorf("prompt length %d exceeds budget+overhead", len(prompt))
	}
}
