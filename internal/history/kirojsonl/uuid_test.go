package kirojsonl

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/textutil"
)

// TestDecodeLine_DerivesDeterministicUUID pins R202606d-CR-002: every
// decoded kiro entry must carry a stable, non-empty UUID so MergedSource
// can dedup overlapping pagination windows (merged.Source bypasses dedup
// entirely for empty-UUID entries). Decoding the same line twice must
// yield the identical UUID, and the value must match the canonical
// textutil.DeriveLegacyUUID derivation the Claude JSONL reader uses.
func TestDecodeLine_DerivesDeterministicUUID(t *testing.T) {
	t.Parallel()
	line := []byte(promptLine("hello world", 1779081689))

	e1, ok1 := decodeLine(line, 0, 0)
	e2, ok2 := decodeLine(line, 0, 0)
	if !ok1 || !ok2 {
		t.Fatalf("decodeLine returned ok=false (%v,%v)", ok1, ok2)
	}
	if e1.UUID == "" {
		t.Fatal("decodeLine produced an empty UUID — merged dedup will bypass this entry")
	}
	if e1.UUID != e2.UUID {
		t.Errorf("UUID not deterministic across decodes: %q vs %q", e1.UUID, e2.UUID)
	}
	// Must match the canonical derivation (Time/Type/Summary/Detail) so dedup
	// keys line up with any other source that ingests the same logical
	// message. Per #2336 the real detail is folded into the hash.
	want := textutil.DeriveLegacyUUID(e1.Time, e1.Type, e1.Summary, e1.Detail)
	if e1.UUID != want {
		t.Errorf("UUID = %q, want canonical DeriveLegacyUUID = %q", e1.UUID, want)
	}
}

// TestDecodeLine_DistinctContentDistinctUUID guards against UUID
// collisions: two lines that differ in content (and thus summary) must
// hash to different UUIDs, otherwise dedup would wrongly collapse them.
func TestDecodeLine_DistinctContentDistinctUUID(t *testing.T) {
	t.Parallel()
	a, okA := decodeLine([]byte(promptLine("first message", 1779081689)), 0, 0)
	b, okB := decodeLine([]byte(promptLine("second message", 1779081689)), 0, 0)
	if !okA || !okB {
		t.Fatalf("decodeLine returned ok=false (%v,%v)", okA, okB)
	}
	if a.UUID == "" || b.UUID == "" {
		t.Fatal("decodeLine produced an empty UUID")
	}
	if a.UUID == b.UUID {
		t.Errorf("distinct content produced identical UUID %q — dedup would collapse unrelated entries", a.UUID)
	}
}

// TestDecodeLine_SameSummaryDifferentDetailDistinctUUID pins #2336: two kiro
// turns in the same wall-clock second whose first 120 runes (the Summary cap)
// are identical but whose detail tail (runes 120..16000) differs must derive
// DISTINCT UUIDs. Previously decodeLine passed detail="" to DeriveLegacyUUID,
// so both turns collided on the same UUID and MergedSource's UUID-first dedup
// silently dropped the second one before the detail-aware contentKey check.
func TestDecodeLine_SameSummaryDifferentDetailDistinctUUID(t *testing.T) {
	t.Parallel()
	// Shared 120-rune prefix (the Summary cap), then differing tails that
	// land in the Detail (runes 120..16000).
	prefix := strings.Repeat("a", 120)
	textA := prefix + strings.Repeat("b", 500)
	textB := prefix + strings.Repeat("c", 500)
	const sameSec = 1779081689

	a, okA := decodeLine([]byte(promptLine(textA, sameSec)), 0, 0)
	b, okB := decodeLine([]byte(promptLine(textB, sameSec)), 0, 0)
	if !okA || !okB {
		t.Fatalf("decodeLine returned ok=false (%v,%v)", okA, okB)
	}
	// Sanity: summaries must be identical (both truncated to the 120-rune
	// prefix) and the time the same — this is the collision precondition.
	if a.Summary != b.Summary {
		t.Fatalf("test precondition broken: summaries differ (%q vs %q)", a.Summary, b.Summary)
	}
	if a.Time != b.Time {
		t.Fatalf("test precondition broken: times differ (%d vs %d)", a.Time, b.Time)
	}
	if a.Detail == b.Detail {
		t.Fatalf("test precondition broken: details identical")
	}
	if a.UUID == b.UUID {
		t.Errorf("#2336: same summary + same second + different detail collided on UUID %q; "+
			"the second turn would be silently dropped by MergedSource dedup", a.UUID)
	}
}

// TestDecodeLine_AssistantBorrowedTimestampHasUUID verifies the
// borrowed-timestamp assistant path (no meta) also gets a UUID, since
// those are the most common real-world kiro records.
func TestDecodeLine_AssistantBorrowedTimestampHasUUID(t *testing.T) {
	t.Parallel()
	// AssistantMessage with no meta — borrows lastPromptMS+1.
	line := []byte(`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"a","content":[{"kind":"text","data":"reply"}]}}`)
	e, ok := decodeLine(line, 1779081689000, 0)
	if !ok {
		t.Fatal("decodeLine returned ok=false for borrowed-timestamp assistant")
	}
	if e.UUID == "" {
		t.Fatal("borrowed-timestamp assistant entry has empty UUID")
	}
}
