package server

import (
	"sort"
	"testing"
)

func set(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// TestRemovedProjectNames pins the pure set-diff extracted out of
// startProjectScanLoop for ARCH-SVR-2 (#460): given the previous and current
// project name sets, it must return exactly the names that disappeared, with
// no false positives for added or unchanged projects.
func TestRemovedProjectNames(t *testing.T) {
	tests := []struct {
		name    string
		old     map[string]struct{}
		current map[string]struct{}
		want    []string
	}{
		{name: "no change", old: set("a", "b"), current: set("a", "b"), want: nil},
		{name: "one removed", old: set("a", "b"), current: set("a"), want: []string{"b"}},
		{name: "all removed", old: set("a", "b"), current: set(), want: []string{"a", "b"}},
		{name: "only added", old: set("a"), current: set("a", "b"), want: nil},
		{name: "removed and added", old: set("a", "b"), current: set("b", "c"), want: []string{"a"}},
		{name: "empty old", old: set(), current: set("a"), want: nil},
		{name: "nil old", old: nil, current: set("a"), want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := removedProjectNames(tc.old, tc.current)
			sort.Strings(got)
			if len(got) != len(tc.want) {
				t.Fatalf("removedProjectNames = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("removedProjectNames = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
