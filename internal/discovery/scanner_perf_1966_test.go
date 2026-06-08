package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestGetCachedPrompt_LockFreeGenRefresh verifies that a cache hit refreshes
// the entry's generation without taking the write lock and survives eviction
// across many scan generations (PERF-6 #1966). The hit path must keep the
// entry's gen current so evictPromptCache does not drop a still-hot entry.
func TestGetCachedPrompt_LockFreeGenRefresh(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const path = "/p/hot.jsonl"
	const mtime int64 = 42

	sc.setCachedPrompt(path, mtime, "hot")

	// Advance the generation several times; each hit must bump the entry's gen
	// to the current generation so it is never more than one gen stale.
	for i := 0; i < 5; i++ {
		sc.promptCache.generation.Add(1)
		got, ok := sc.getCachedPrompt(path, mtime)
		if !ok || got != "hot" {
			t.Fatalf("iter %d: getCachedPrompt = %q, %v; want hot,true", i, got, ok)
		}
	}

	sc.promptCache.RLock()
	e := sc.promptCache.entries[path]
	cur := sc.promptCache.generation.Load()
	sc.promptCache.RUnlock()
	if e.gen.Load() != cur {
		t.Errorf("entry gen = %d, want current generation %d", e.gen.Load(), cur)
	}
}

// TestGetCachedPrompt_ConcurrentHits exercises many goroutines refreshing the
// same entry concurrently while the generation advances. With -race this
// catches any unsynchronised access — the gen refresh must be a lock-free
// atomic Store, never a racy map write.
func TestGetCachedPrompt_ConcurrentHits(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const path = "/p/shared.jsonl"
	const mtime int64 = 7
	sc.setCachedPrompt(path, mtime, "shared")

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if got, ok := sc.getCachedPrompt(path, mtime); !ok || got != "shared" {
					t.Errorf("getCachedPrompt = %q,%v; want shared,true", got, ok)
					return
				}
			}
		}()
	}
	// Concurrently advance the generation, as Scan would each cycle.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			sc.promptCache.generation.Add(1)
		}
	}()
	wg.Wait()
}

// TestExtractLastPromptUncached_PromptStraddlesTailBoundary guards the PERF-7
// single-pass refactor: a user prompt whose JSONL record straddles the 512KB
// tail boundary must still be found. alignTailOffset partitions the file at a
// record boundary so neither the tail scan nor the head fallback splits — and
// drops — the boundary record.
func TestExtractLastPromptUncached_PromptStraddlesTailBoundary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "straddle.jsonl")

	// A no-text user line (tool_result only) so the tail yields nothing.
	noText := `{"type":"user","timestamp":"2026-01-01T01:00:00Z","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}`
	// The real prompt line.
	real := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"boundary prompt"}}`

	const tailSize = 512 * 1024
	var sb strings.Builder
	// Pad the head so the real prompt's record crosses the (size-512KB) mark.
	for sb.Len() < tailSize-len(real)/2 {
		sb.WriteString(noText)
		sb.WriteByte('\n')
	}
	sb.WriteString(real)
	sb.WriteByte('\n')
	// Trailing tool-result tail (no text) so only the head holds a prompt.
	for i := 0; i < 1000; i++ {
		sb.WriteString(noText)
		sb.WriteByte('\n')
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := extractLastPromptUncached(path, fi.Size()); got != "boundary prompt" {
		t.Errorf("extractLastPromptUncached = %q, want boundary prompt", got)
	}
}

// TestExtractLastPromptUncached_HeadPromptManyCases is a small table over file
// shapes: prompt only in head (tail all tool_result), prompt in tail, and a
// short file under the tail window. All must resolve the last text prompt.
func TestExtractLastPromptUncached_HeadPromptManyCases(t *testing.T) {
	t.Parallel()
	noText := `{"type":"user","timestamp":"2026-01-01T01:00:00Z","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}`
	mkPrompt := func(s string) string {
		return fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":%q}}`, s)
	}

	cases := []struct {
		name string
		// build returns the file content and the expected last prompt.
		build func() (string, string)
	}{
		{
			name: "short_file",
			build: func() (string, string) {
				return mkPrompt("only prompt") + "\n", "only prompt"
			},
		},
		{
			name: "prompt_in_tail",
			build: func() (string, string) {
				var sb strings.Builder
				for sb.Len() < 700*1024 {
					sb.WriteString(noText)
					sb.WriteByte('\n')
				}
				sb.WriteString(mkPrompt("tail prompt"))
				sb.WriteByte('\n')
				return sb.String(), "tail prompt"
			},
		},
		{
			name: "prompt_in_head_only",
			build: func() (string, string) {
				var sb strings.Builder
				sb.WriteString(mkPrompt("head prompt"))
				sb.WriteByte('\n')
				for sb.Len() < 700*1024 {
					sb.WriteString(noText)
					sb.WriteByte('\n')
				}
				return sb.String(), "head prompt"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "f.jsonl")
			content, want := tc.build()
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := extractLastPromptUncached(path, fi.Size()); got != want {
				t.Errorf("extractLastPromptUncached = %q, want %q", got, want)
			}
		})
	}
}
