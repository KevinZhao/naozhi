package session

import (
	"reflect"
	"testing"

	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
)

// TestProjectListEntry_MirrorsProjectsListEntry pins that the palette fields
// dashboard.js reads from stats.projects (stableKey / dir_mtime / config /
// created_at) carry the exact same JSON tags as /api/projects'
// projectsListEntry. The two lists feed the same frontend consumers; a tag
// drift on either side silently breaks continue-session / ordering / labels.
func TestProjectListEntry_MirrorsProjectsListEntry(t *testing.T) {
	local := reflect.TypeOf(projectListEntry{})
	ref := dashproject.ProjectsListEntryType()
	for _, name := range []string{"StableKey", "DirModTime", "Config", "CreatedAt", "Name", "Path"} {
		lf, ok := local.FieldByName(name)
		if !ok {
			t.Fatalf("projectListEntry lacks field %s", name)
		}
		rf, ok := ref.FieldByName(name)
		if !ok && name == "CreatedAt" {
			// /api/projects carries created_at inside config only.
			continue
		}
		if !ok {
			t.Fatalf("projectsListEntry lacks field %s", name)
		}
		if lf.Tag.Get("json") != rf.Tag.Get("json") {
			t.Errorf("%s json tag drift: session=%q projects=%q", name, lf.Tag.Get("json"), rf.Tag.Get("json"))
		}
		if lf.Type != rf.Type {
			t.Errorf("%s type drift: session=%s projects=%s", name, lf.Type, rf.Type)
		}
	}
}
