package project

import (
	"sort"
	"strings"
	"testing"
)

// oldSortEntries is the original in-place comparator (two ToLower per compare).
// Kept here only to pin that the optimized sortEntries produces an identical
// ordering (R202606c-PERF-001).
func oldSortEntries(entries []listEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
}

func namesOf(entries []listEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func TestSortEntries_MatchesOldComparator(t *testing.T) {
	t.Parallel()
	mixed := []listEntry{
		{Name: "Zebra.txt"},
		{Name: "alpha", IsDir: true},
		{Name: "Bravo", IsDir: true},
		{Name: "apple.go"},
		{Name: "APPLE.md"},
		{Name: "charlie", IsDir: true},
		{Name: "README"},
		{Name: "readme.lower"},
		{Name: "zeta.txt"},
		{Name: ".hidden"},
		{Name: ".Config", IsDir: true},
	}

	// Independent copies for each comparator.
	want := make([]listEntry, len(mixed))
	copy(want, mixed)
	oldSortEntries(want)

	got := make([]listEntry, len(mixed))
	copy(got, mixed)
	sortEntries(got)

	wantNames := namesOf(want)
	gotNames := namesOf(got)
	if len(wantNames) != len(gotNames) {
		t.Fatalf("length mismatch: want %d got %d", len(wantNames), len(gotNames))
	}
	for i := range wantNames {
		if wantNames[i] != gotNames[i] {
			t.Fatalf("order mismatch at %d: want %v, got %v", i, wantNames, gotNames)
		}
	}

	// Sanity: all dirs precede all files.
	seenFile := false
	for _, e := range got {
		if !e.IsDir {
			seenFile = true
		} else if seenFile {
			t.Fatalf("dir %q appears after a file — dirs-first violated: %v", e.Name, gotNames)
		}
	}
}

func TestSortEntries_Empty(t *testing.T) {
	t.Parallel()
	var entries []listEntry
	sortEntries(entries) // must not panic
	if len(entries) != 0 {
		t.Fatalf("expected empty, got %v", entries)
	}
}
