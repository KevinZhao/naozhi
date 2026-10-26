package project

import (
	"slices"
	"testing"
)

func namesOf(entries []listEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

// Directories first, then case-insensitive by name.
func TestSortEntries_DirsFirstCaseInsensitive(t *testing.T) {
	t.Parallel()
	entries := []listEntry{
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
	sortEntries(entries)
	want := []string{".Config", "alpha", "Bravo", "charlie", ".hidden", "apple.go", "APPLE.md", "README", "readme.lower", "Zebra.txt", "zeta.txt"}
	if got := namesOf(entries); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
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

// #2433 item 5: names that collide under case-folding (Makefile / makefile)
// must sort identically regardless of the ReadDir input order, otherwise the
// listing "jitters" between refreshes.
func TestSortEntries_CaseFoldTiesDeterministic(t *testing.T) {
	t.Parallel()
	perms := [][]string{
		{"Makefile", "makefile", "MAKEFILE"},
		{"makefile", "MAKEFILE", "Makefile"},
		{"MAKEFILE", "Makefile", "makefile"},
	}
	var first []string
	for _, p := range perms {
		entries := make([]listEntry, len(p))
		for i, n := range p {
			entries[i] = listEntry{Name: n}
		}
		sortEntries(entries)
		got := namesOf(entries)
		if first == nil {
			first = got
			continue
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("order depends on input order: %v vs %v (input %v)", first, got, p)
			}
		}
	}
}
