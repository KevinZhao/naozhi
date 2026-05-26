package config_test

import (
	"go/build"
	"strings"
	"testing"
)

// TestConfigDoesNotImportSession enforces R222-ARCH-3: internal/config used to
// reverse-import internal/session for one constant (session.DefaultMaxProcs).
// The constant moved to internal/sessionconst, and config now imports that
// instead. If a future change accidentally re-introduces the session import,
// the dependency direction (high-level -> low-level) flips again and we lose
// the ability to use config from session-package tests without import cycles.
func TestConfigDoesNotImportSession(t *testing.T) {
	pkg, err := build.Default.Import("github.com/naozhi/naozhi/internal/config", ".", build.ImportComment)
	if err != nil {
		t.Fatalf("Import internal/config: %v", err)
	}
	for _, imp := range pkg.Imports {
		if imp == "github.com/naozhi/naozhi/internal/session" {
			t.Errorf("internal/config still imports internal/session; "+
				"see R222-ARCH-3 — use internal/sessionconst for shared "+
				"constants. Imports: %v", pkg.Imports)
		}
		// Defensive: catch any nested re-import path we did not anticipate.
		if strings.HasPrefix(imp, "github.com/naozhi/naozhi/internal/session/") {
			t.Errorf("internal/config imports session sub-package %q; "+
				"this also creates a reverse dependency. Move the shared "+
				"value into internal/sessionconst instead.", imp)
		}
	}
}
