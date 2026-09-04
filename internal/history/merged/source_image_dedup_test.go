package merged

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// These tests pin the image-only user-message dedup fix (dashboard "顺序
// 错乱" bug): both tiers emit an image-only user message with Detail==""
// (buildUserEntry truncates the empty text; discovery's parseHistoryLine
// likewise), so the Detail-based contentKey abstained on BOTH sides and —
// the two tiers' UUIDs never coinciding by construction — every image-only
// message rendered twice, skewed apart by up to contentSkewLeadMS (~1.4s
// measured), with assistant output interleaving between the two copies.
//
// The fix keys Detail-empty entries on their Images list: both tiers derive
// thumbnails from the identical original bytes through the same
// deterministic pipeline (cli.MakeThumbnail at maxDim=600), so the data
// URIs match byte-for-byte for the same logical message.

const (
	thumbA = "data:image/jpeg;base64,AAAA"
	thumbB = "data:image/jpeg;base64,BBBB"
)

// TestContentKey_ImageOnlyUserMessage: Detail=="" + Images → a non-empty
// identity that matches across tiers and separates different image sets.
func TestContentKey_ImageOnlyUserMessage(t *testing.T) {
	local := clievent.EventEntry{Type: "user", Detail: "", Images: []string{thumbA}}
	fallback := clievent.EventEntry{Type: "user", Detail: "", Images: []string{thumbA}}
	if k := contentKey(local); k == "" {
		t.Fatal("contentKey(image-only user entry) = \"\" — image identity missing, duplicate bubbles return")
	}
	if contentKey(local) != contentKey(fallback) {
		t.Error("same thumbnails must produce the same content key across tiers")
	}
	other := clievent.EventEntry{Type: "user", Detail: "", Images: []string{thumbB}}
	if contentKey(local) == contentKey(other) {
		t.Error("different thumbnails must NOT share a content key (would collapse distinct messages)")
	}
	// typeOfKey must still recover "user" so skewWindowFor picks the
	// user-direction (local-leads) window.
	if got := typeOfKey(contentKey(local)); got != "user" {
		t.Errorf("typeOfKey = %q, want \"user\"", got)
	}
	// Entries with neither Detail nor Images (e.g. a local `result` event)
	// must keep abstaining.
	if k := contentKey(clievent.EventEntry{Type: "result"}); k != "" {
		t.Errorf("contentKey(no Detail, no Images) = %q, want \"\"", k)
	}
	// Detail-bearing entries must be keyed by Detail alone — Images must not
	// perturb the existing text identity (text+image messages already dedup
	// via Detail on both tiers).
	withText := clievent.EventEntry{Type: "user", Detail: "hello", Images: []string{thumbA}}
	textOnly := clievent.EventEntry{Type: "user", Detail: "hello"}
	if contentKey(withText) != contentKey(textOnly) {
		t.Error("Detail-bearing key must ignore Images (text+image vs text-only same Detail)")
	}
}

// TestMerged_ImageOnlyUserDedup: the end-to-end shape of the bug. Local ring
// persisted the image-only user message at T (stamped at stdin write);
// the Claude JSONL twin lands ~1.4s later with a different UUID. The merge
// must emit exactly ONE user bubble — the local one (richer render path).
func TestMerged_ImageOnlyUserDedup(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "local-user", Time: 5000, Type: "user", Summary: " [+1 image(s)]", Images: []string{thumbA}},
			{UUID: "local-text", Time: 6000, Type: "text", Summary: "reply", Detail: "reply"},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			// Claude stamps the user record later (cold-spawn worst case ~1.4s).
			{UUID: "claude-user", Time: 6391, Type: "user", Images: []string{thumbA}},
		}},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	users := 0
	for _, e := range got {
		if e.Type == "user" {
			users++
			if e.UUID != "local-user" {
				t.Errorf("surviving user entry UUID = %q, want the LOCAL copy (richer fidelity)", e.UUID)
			}
		}
	}
	if users != 1 {
		t.Fatalf("got %d user bubbles, want 1 — image-only message duplicated across tiers: %+v", users, got)
	}
}

// TestMerged_ImageOnlyDistinctMessagesSurvive: two DIFFERENT image-only
// messages inside the skew window must both render — the identity is the
// thumbnail content, not "any image-only message nearby".
func TestMerged_ImageOnlyDistinctMessagesSurvive(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "local-user-a", Time: 5000, Type: "user", Images: []string{thumbA}},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			// Different image, 400ms later — inside the user skew window but
			// NOT the same message.
			{UUID: "claude-user-b", Time: 5400, Type: "user", Images: []string{thumbB}},
		}},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 — distinct image-only messages must not collapse: %+v", len(got), got)
	}
}

// TestMerged_ImageOnlyOutsideSkewWindowSurvives: the same thumbnails far
// apart in time are two separate sends of the same picture, not one turn.
func TestMerged_ImageOnlyOutsideSkewWindowSurvives(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "local-user-1", Time: 5000, Type: "user", Images: []string{thumbA}},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			// Same image re-sent 10s later — outside contentSkewLeadMS.
			{UUID: "claude-user-2", Time: 15000, Type: "user", Images: []string{thumbA}},
		}},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 — re-sent picture outside skew window must survive: %+v", len(got), got)
	}
}
