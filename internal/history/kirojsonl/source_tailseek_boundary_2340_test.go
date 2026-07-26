package kirojsonl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseFile_TailSeekBoundaryAlignedKeepsFirstLine pins #2340: when the tail
// window's seek offset (Size-maxFileBytes) lands exactly on a record boundary
// (the byte before the offset is '\n'), the first in-window line is a complete,
// valid JSONL record and must NOT be discarded as a partial. The old code
// unconditionally dropped the first line, silently losing the oldest in-window
// turn — which kirojsonl is the only source for.
//
// We exercise parseFile directly (not LoadBefore) so the newest-`limit` tail
// trim does not hide the oldest in-window record we are asserting on.
//
// Construction: the tail region is built to be exactly maxFileBytes long, so
// the seek offset falls precisely at the head/tail seam. The first byte of the
// tail region is the start of the BOUNDARY prompt line; the byte immediately
// before it is the head's trailing '\n'.
func TestParseFile_TailSeekBoundaryAlignedKeepsFirstLine(t *testing.T) {
	t.Parallel()
	body, boundaryTS := buildBoundaryAlignedSession(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "boundary.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	s := New(dir, func() string { return "boundary" })
	got := s.parseFile(context.Background(), f, 0)

	var sawBoundary, sawNewest, sawOldest bool
	for _, e := range got {
		switch {
		case e.Time == boundaryTS*1000:
			sawBoundary = true
		case strings.Contains(e.Summary, "NEWEST"):
			sawNewest = true
		case strings.Contains(e.Summary, "OLDEST"):
			sawOldest = true
		}
	}
	if !sawBoundary {
		t.Errorf("#2340: boundary-aligned first window line was dropped; want it surfaced")
	}
	if !sawNewest {
		t.Errorf("newest tail record not surfaced (sanity)")
	}
	if sawOldest {
		t.Errorf("head record outside the window leaked in")
	}
}

// buildBoundaryAlignedSession returns a kiro session body whose tail window
// (the last maxFileBytes) begins exactly on a record boundary, plus the unix
// timestamp of the boundary (first in-window) prompt.
func buildBoundaryAlignedSession(t *testing.T) (body string, boundaryTS int64) {
	t.Helper()
	boundaryTS = 1_700_000_500
	boundary := promptLine("BOUNDARY first window line must survive", boundaryTS) + "\n"
	newest := promptLine("NEWEST must be visible", 1_700_009_999) + "\n"
	fillerBase := promptLine(strings.Repeat("x", 4096), 1_700_000_600) + "\n"

	var tail strings.Builder
	tail.WriteString(boundary)
	for tail.Len()+len(fillerBase)+len(newest) <= maxFileBytes {
		tail.WriteString(fillerBase)
	}
	// Reserve space for the newest line (the last in-window record) and pad the
	// gap with a single junk full-line so the tail region is exactly
	// maxFileBytes once newest is appended.
	remaining := maxFileBytes - len(newest) - tail.Len()
	if remaining < 0 {
		t.Fatalf("tail construction overshot: remaining=%d", remaining)
	}
	if remaining > 0 {
		// A line of 'x' bytes ending in '\n' decodes as junk and is skipped,
		// which is fine — it only needs to occupy bytes inside the window.
		tail.WriteString(strings.Repeat("x", remaining-1) + "\n")
	}
	tail.WriteString(newest)
	if tail.Len() != maxFileBytes {
		t.Fatalf("tail length = %d, want exactly maxFileBytes=%d", tail.Len(), maxFileBytes)
	}

	// Head: an OLDEST record that lives entirely outside the tail window and a
	// trailing '\n'. Its length pushes Size past maxFileBytes and sets the seek
	// offset to len(head), landing on the head's trailing '\n' (the boundary).
	head := promptLine("OLDEST head must be dropped", 1_699_999_000) + "\n"
	return head + tail.String(), boundaryTS
}
