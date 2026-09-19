package codexjsonl

import (
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/textutil"
)

// TestSource_UUID_DistinctDetailSameSummary covers #2336: two codex lines
// with the same timestamp and the same 120-rune summary prefix but differing
// detail tails must derive distinct UUIDs. Deriving with detail="" made them
// collide, so merged.Source's UUID-first dedup silently dropped the second
// turn. kiro (kirojsonl) already folds the real detail into the hash; codex
// must match.
func TestSource_UUID_DistinctDetailSameSummary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "019ee988-da7f-7821-b6d1-7b74a7db62d7"
	prefix := strings.Repeat("a", 120) // fills the whole Summary cap
	writeRollout(t, dir, sid, []string{
		`{"timestamp":"2026-06-21T09:35:21.000Z","type":"event_msg","payload":{"type":"agent_message","message":"` + prefix + `-first tail"}}`,
		`{"timestamp":"2026-06-21T09:35:21.000Z","type":"event_msg","payload":{"type":"agent_message","message":"` + prefix + `-second tail"}}`,
	})

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Summary != got[1].Summary || got[0].Time != got[1].Time {
		t.Fatalf("fixture must share summary+time: %+v vs %+v", got[0], got[1])
	}
	if got[0].Detail == got[1].Detail {
		t.Fatalf("fixture must differ in detail: %q", got[0].Detail)
	}
	if got[0].UUID == "" || got[1].UUID == "" {
		t.Fatalf("empty UUID: %q / %q", got[0].UUID, got[1].UUID)
	}
	if got[0].UUID == got[1].UUID {
		t.Errorf("same-ts same-summary entries with different detail share UUID %q — merged.Source would drop one", got[0].UUID)
	}
	// Pin the canonical derivation (kirojsonl/uuid_test.go does the same):
	// distinctness alone would let a refactor drift to any detail-sensitive
	// hash while silently breaking the cross-source convention.
	for i, e := range got {
		if want := textutil.DeriveLegacyUUID(e.Time, e.Type, e.Summary, e.Detail); e.UUID != want {
			t.Errorf("entry %d: UUID %q, want canonical DeriveLegacyUUID %q", i, e.UUID, want)
		}
	}
}
