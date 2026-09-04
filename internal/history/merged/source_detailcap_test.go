package merged

import (
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/textutil"
)

// TestMerged_CrossSourceContentDedup_DetailCapMismatch: the two tiers
// truncate the same turn at different bounds — live at
// cli.EventDetailMaxRunes (2000), fallback readers at
// history.DetailMaxRunes (16000). Before contentKey normalised to the
// tighter cap, any turn longer than the live cap mismatched at rune 2000
// and (the tiers' UUIDs never coinciding by construction) rendered twice.
func TestMerged_CrossSourceContentDedup_DetailCapMismatch(t *testing.T) {
	text := strings.Repeat("x", cli.EventDetailMaxRunes+1000) // longer than the live cap
	localDetail := textutil.TruncateRunes(text, cli.EventDetailMaxRunes)
	fallbackDetail := textutil.TruncateRunes(text, history.DetailMaxRunes)
	if localDetail == fallbackDetail {
		t.Fatalf("fixture must exercise the cap mismatch: details are equal")
	}
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "nativecryptorand0000000000000000", Time: 100, Type: "text", Summary: "long", Detail: localDetail},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "derivedfallbackuuid0000000000000", Time: 100, Type: "text", Summary: "long", Detail: fallbackDetail},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (>%d-rune turn must dedup across the cap mismatch)", len(got), cli.EventDetailMaxRunes)
	}
	if got[0].UUID != "nativecryptorand0000000000000000" {
		t.Errorf("local entry should win dedup, got %+v", got[0])
	}
}

// TestMerged_CrossSourceContentDedup_LongDistinctNotCollapsed: two long
// turns that already differ INSIDE the live-visible prefix must survive
// the normalisation — re-truncating to the tighter cap may only equalise
// tails past rune 2000, never differences the user can see.
func TestMerged_CrossSourceContentDedup_LongDistinctNotCollapsed(t *testing.T) {
	base := strings.Repeat("x", cli.EventDetailMaxRunes+1000)
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "u1", Time: 100, Type: "text", Summary: "long", Detail: textutil.TruncateRunes("A"+base, cli.EventDetailMaxRunes)},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "u2", Time: 100, Type: "text", Summary: "long", Detail: textutil.TruncateRunes("B"+base, history.DetailMaxRunes)},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (visible-prefix difference must not collapse)", len(got))
	}
}
