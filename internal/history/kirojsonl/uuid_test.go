package kirojsonl

import (
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
	// Must match the canonical derivation (Time/Type/Summary, empty detail)
	// so dedup keys line up with any other source that ingests the same
	// logical message.
	want := textutil.DeriveLegacyUUID(e1.Time, e1.Type, e1.Summary, "")
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
