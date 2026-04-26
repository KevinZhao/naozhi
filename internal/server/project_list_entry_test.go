package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProjectListEntry_JSONShape locks the wire format of projectListEntry so
// the R70-PERF-M1 map→struct migration does not silently break dashboard.js
// consumers (p.name / p.path / p.node / p.favorite / p.git_remote_url /
// p.github). The prior map[string]any literal emitted only the keys that were
// actually set; the struct mirrors that with omitempty on the bool + optional
// string fields. Required fields (name, path, node) are always emitted even
// when empty so downstream `p.name === name && (p.node || 'local') ===
// nodeID` comparisons work for rows with empty paths.
func TestProjectListEntry_JSONShape(t *testing.T) {
	cases := []struct {
		name     string
		entry    projectListEntry
		wantHas  []string
		wantMiss []string
	}{
		{
			name: "full local entry",
			entry: projectListEntry{
				Name: "work", Path: "/home/u/work", Node: "local",
				Favorite: true, GitRemoteURL: "https://github.com/o/r", GitHub: true,
			},
			wantHas: []string{`"name":"work"`, `"path":"/home/u/work"`, `"node":"local"`, `"favorite":true`, `"git_remote_url":"https://github.com/o/r"`, `"github":true`},
		},
		{
			name:     "minimal entry (favorite/github false, no remote URL)",
			entry:    projectListEntry{Name: "n", Path: "/p", Node: "remote-1"},
			wantHas:  []string{`"name":"n"`, `"path":"/p"`, `"node":"remote-1"`},
			wantMiss: []string{`"favorite"`, `"git_remote_url"`, `"github"`},
		},
		{
			name:     "remote entry without favorite but with git URL",
			entry:    projectListEntry{Name: "r", Path: "/x", Node: "node2", GitRemoteURL: "git@github.com:o/r.git"},
			wantHas:  []string{`"git_remote_url":"git@github.com:o/r.git"`},
			wantMiss: []string{`"favorite"`, `"github"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, err := json.Marshal(tc.entry)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := string(buf)
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("json = %s\nmissing %q", got, want)
				}
			}
			for _, unwanted := range tc.wantMiss {
				if strings.Contains(got, unwanted) {
					t.Errorf("json = %s\nshould omit %q (zero-value fields must have omitempty)", got, unwanted)
				}
			}
		})
	}
}
