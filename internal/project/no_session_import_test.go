package project_test

import (
	"go/build"
	"strings"
	"testing"
)

// TestProjectDoesNotImportSession enforces R260528-ARCH-12 (#1373):
// internal/project (the domain layer) used to reverse-import internal/session
// (the routing layer) solely to name PlannerDataSource / ProjectBinding in its
// datasource.go adapter. Those contract types moved to the neutral leaf
// internal/projectapi, and project now imports that instead.
//
// If a future change re-introduces the session import, the dependency
// direction (domain -> routing) flips back and the cycle this decoupling broke
// returns. session keeps type aliases (session.PlannerDataSource =
// projectapi.DataSource) so callers wiring project.NewDataSource into
// session.NewKeyResolver still compile — but project itself must depend only on
// projectapi.
func TestProjectDoesNotImportSession(t *testing.T) {
	pkg, err := build.Default.Import("github.com/naozhi/naozhi/internal/project", ".", build.ImportComment)
	if err != nil {
		t.Fatalf("Import internal/project: %v", err)
	}
	for _, imp := range pkg.Imports {
		if imp == "github.com/naozhi/naozhi/internal/session" {
			t.Errorf("internal/project still imports internal/session; "+
				"see R260528-ARCH-12 (#1373) — use internal/projectapi for the "+
				"shared ProjectBinding / DataSource contract. Imports: %v", pkg.Imports)
		}
		// Defensive: catch any session sub-package re-import path too.
		if strings.HasPrefix(imp, "github.com/naozhi/naozhi/internal/session/") {
			t.Errorf("internal/project imports session sub-package %q; "+
				"this also creates a reverse dependency. Move the shared "+
				"contract into internal/projectapi instead.", imp)
		}
	}
}
