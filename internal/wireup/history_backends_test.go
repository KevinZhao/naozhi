package wireup

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/history/claudejsonl"
)

// TestHistoryBackends_ClaudeFactoryRegistered pins the #3020 S15b seam:
// session's claudeTranscriptLoader resolves the "claude" backend through
// history.PickFactory rather than importing internal/discovery, so the
// factory this package blank-imports (history_backends.go) must actually be
// registered and must actually produce a *claudejsonl.Source — a wireup
// mistake that dropped or mis-registered the blank import would otherwise
// only surface at runtime as silently empty history.
func TestHistoryBackends_ClaudeFactoryRegistered(t *testing.T) {
	factory := history.PickFactory("claude")
	if factory == nil {
		t.Fatal(`history.PickFactory("claude") returned nil — the blank import of ` +
			`internal/history/claudejsonl in history_backends.go did not register it`)
	}
	src := factory(fakeSessionView{}, history.Wiring{ClaudeDir: t.TempDir()})
	if _, ok := src.(*claudejsonl.Source); !ok {
		t.Fatalf(`history.PickFactory("claude")(...) = %T, want *claudejsonl.Source`, src)
	}
}

// TestHistoryBackends_EveryProfileHasFactory guards the pairing nothing else
// checks at startup: a backend profile registered without a blank-imported
// history factory boots fine and then shows every session's history as empty.
func TestHistoryBackends_EveryProfileHasFactory(t *testing.T) {
	NewBoot().EnsureCLIBackends()
	profiles := backend.All()
	if len(profiles) == 0 {
		t.Fatal("backend.All() is empty after EnsureCLIBackends; nothing to check")
	}
	for _, p := range profiles {
		if history.PickFactory(p.ID) == nil {
			t.Errorf("backend %q has no history factory; blank-import its history "+
				"package in history_backends.go", p.ID)
		}
	}
}

// fakeSessionView satisfies history.SessionView with fixed values; only
// Workspace/SnapshotChainIDs matter to claudejsonl's factory.
type fakeSessionView struct{}

func (fakeSessionView) SessionKey() string         { return "k" }
func (fakeSessionView) Workspace() string          { return "/ws" }
func (fakeSessionView) SessionID() string          { return "id" }
func (fakeSessionView) SnapshotChainIDs() []string { return []string{"id"} }
