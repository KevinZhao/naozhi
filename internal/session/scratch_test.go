package session

import (
	"strings"
	"testing"
	"time"
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
