package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractLastPromptUncached_FallbackBounded pins #2227: the head fallback
// (taken when the 512KB tail holds no text prompt) must not read the entire
// file. A prompt buried in the file's middle — beyond 512KB from both the
// start and the end, with a tool_result-only tail — must NOT be surfaced,
// because both the bounded tail scan and the bounded head scan skip it. Before
// the fix the fallback did Seek(0)+full scan and would have found it, reading
// the whole 10MB file on every preview refresh.
func TestExtractLastPromptUncached_FallbackBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "buried.jsonl")

	noText := `{"type":"user","timestamp":"2026-01-01T01:00:00Z","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}`
	buried := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"buried prompt"}}`

	const tailSize = 512 * 1024
	var sb strings.Builder
	// Head padding > tailSize so the head scan stops before the buried prompt.
	for sb.Len() < tailSize+64*1024 {
		sb.WriteString(noText)
		sb.WriteByte('\n')
	}
	sb.WriteString(buried)
	sb.WriteByte('\n')
	// Tail padding > tailSize so the tail scan also never reaches it.
	for {
		sb.WriteString(noText)
		sb.WriteByte('\n')
		if sb.Len() > 2*tailSize+128*1024 {
			break
		}
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := extractLastPromptUncached(path, fi.Size()); got != "" {
		t.Errorf("extractLastPromptUncached = %q, want \"\" (buried prompt beyond both bounded windows must not be read)", got)
	}
}

// BenchmarkExtractLastPromptUncached_FallbackLargeFile shows the fallback no
// longer scales with total file size: a 10MB file whose tail is all
// tool_result lines triggers the head fallback, which now reads at most the
// first 512KB instead of all 10MB.
func BenchmarkExtractLastPromptUncached_FallbackLargeFile(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "big.jsonl")

	noText := `{"type":"user","timestamp":"2026-01-01T01:00:00Z","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}`
	head := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"head prompt"}}`
	var sb strings.Builder
	sb.WriteString(head)
	sb.WriteByte('\n')
	for sb.Len() < 10<<20 {
		sb.WriteString(noText)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := extractLastPromptUncached(path, fi.Size()); got != "head prompt" {
			b.Fatalf("got %q", got)
		}
	}
}
