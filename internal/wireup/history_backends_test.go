package wireup

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/history"
)

// TestHistoryBackends_EveryProfileHasFactory guards the pairing nothing else
// checks at startup: a backend profile registered without a blank-imported
// history factory boots fine, then only logs a runtime Warn
// (history.WarnMissingFactory) while every session's history shows empty.
func TestHistoryBackends_EveryProfileHasFactory(t *testing.T) {
	RegisterCLIBackends()
	profiles := backend.All()
	if len(profiles) == 0 {
		t.Fatal("backend.All() is empty after RegisterCLIBackends; nothing to check")
	}
	for _, p := range profiles {
		if history.PickFactory(p.ID) == nil {
			t.Errorf("backend %q has no history factory; blank-import its history "+
				"package in history_backends.go", p.ID)
		}
	}
}
