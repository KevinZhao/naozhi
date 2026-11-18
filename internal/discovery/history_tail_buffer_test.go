package discovery

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestLoadHistoryTail_LongLinesMatchForwardRead: lines spanning many chunks,
// lines ending near chunk edges and one over the carry cap read the same from
// the tail, whole and page by page, as LoadHistory reads them forward; a
// second pass reuses the read buffer.
func TestLoadHistoryTail_LongLinesMatchForwardRead(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-long-lines"
	sessionID := "00000000-0000-0000-0000-000000003001"
	sizes := []int{10, 1300 << 10, 100, 300 << 10, 5 << 20, tailChunkSize - 200, tailChunkSize + 1, 50, 600 << 10, 10}
	var lines []string
	for pass := 0; pass < 2; pass++ {
		for i, n := range sizes {
			k := pass*len(sizes) + i
			content := fmt.Sprintf("msg-%02d-", k) + strings.Repeat("x", n)
			lines = append(lines, userJSONLLineAt(content, 1767225600+int64(k)))
		}
	}
	makeSessionJSONL(t, claudeDir, claudefs.ProjectSlug(cwd), sessionID, lines)

	want, err := LoadHistory(claudeDir, sessionID, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != len(lines)-2 {
		t.Fatalf("forward read = %d entries, want %d (the two over the cap dropped)", len(want), len(lines)-2)
	}
	for pass := 0; pass < 2; pass++ {
		got, err := LoadHistoryTailCtx(context.Background(), claudeDir, sessionID, cwd, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("pass %d: tail read differs from forward read: %d vs %d entries", pass, len(got), len(want))
		}
	}

	var paged []clievent.EventEntry
	var before int64
	for {
		page, err := LoadHistoryTailBeforeCtx(context.Background(), claudeDir, sessionID, cwd, before, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(append([]clievent.EventEntry(nil), page...), paged...)
		before = page[0].Time
	}
	if !reflect.DeepEqual(paged, want) {
		t.Fatalf("paged read differs from forward read: %d vs %d entries", len(paged), len(want))
	}
}

// TestLoadHistoryTailText_SkipsThumbnails: the text-only read thumbnails
// nothing and returns the image read's user and text entries without their

// TestParseTail_ShortReadKeepsNoPooledBytes: when the file is shorter than
// the size parseTail was given (it shrank after Stat), the unread part of the
// chunk must not surface lines a pooled buffer kept from another transcript.
func TestParseTail_ShortReadKeepsNoPooledBytes(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/tail-short-read"
	sessionID := "00000000-0000-0000-0000-000000003003"
	lines := []string{userJSONLLineAt("mine-1", 1767225600), userJSONLLineAt("mine-2", 1767225601)}
	_, path := makeSessionJSONL(t, claudeDir, claudefs.ProjectSlug(cwd), sessionID, lines)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	foreign := []byte(userJSONLLineAt("foreign", 1767225500) + "\n")
	for i := 0; i < 20; i++ {
		// Leave a buffer full of another transcript's lines in the pool.
		bp := tailBufPool.Get().(*[]byte)
		for off := 0; off < len(*bp); off += copy((*bp)[off:], foreign) {
		}
		tailBufPool.Put(bp)

		got, err := parseTail(context.Background(), f, st.Size()+100<<10, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, e := range got {
			texts = append(texts, e.Summary)
		}
		if strings.Join(texts, ",") != "mine-1,mine-2" {
			t.Fatalf("pass %d: entries = %q, want only this file's two", i, texts)
		}
	}
}
