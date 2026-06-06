package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestUploadStorePeek_DoesNotConsume(t *testing.T) {
	s := newUploadStore()
	img := cli.ImageData{Data: []byte("original"), MimeType: "image/jpeg"}
	id, err := s.Put("alice", img)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Peek twice — entry must survive both, and the bytes must match.
	for i := 0; i < 2; i++ {
		got := s.Peek(id, "alice")
		if got == nil {
			t.Fatalf("Peek #%d returned nil; entry should still be live", i)
		}
		if string(got.Data) != "original" {
			t.Errorf("Peek #%d data = %q, want original", i, got.Data)
		}
	}

	// The entry must still be takeable (Peek didn't consume it).
	if got := s.Take(id, "alice"); got == nil {
		t.Error("Take after Peek returned nil; Peek must not consume the entry")
	}
}

func TestUploadStorePeek_OwnerIsolation(t *testing.T) {
	s := newUploadStore()
	id, _ := s.Put("alice", cli.ImageData{Data: []byte("secret"), MimeType: "image/jpeg"})
	if got := s.Peek(id, "bob"); got != nil {
		t.Error("Peek must return nil for a non-owner")
	}
	if got := s.Peek("nonexistent", "alice"); got != nil {
		t.Error("Peek must return nil for an unknown id")
	}
}

func TestUploadStorePeek_ReturnsCopy(t *testing.T) {
	s := newUploadStore()
	id, _ := s.Put("alice", cli.ImageData{Data: []byte("abcd"), MimeType: "image/jpeg"})
	got := s.Peek(id, "alice")
	if got == nil {
		t.Fatal("Peek returned nil")
	}
	// Mutate the returned copy; the stored entry must be unaffected.
	got.Data[0] = 'X'
	again := s.Peek(id, "alice")
	if string(again.Data) != "abcd" {
		t.Errorf("mutating Peek's result corrupted the store: %q", again.Data)
	}
}

func TestUploadStoreReplace_UpdatesBytes(t *testing.T) {
	s := newUploadStore()
	id, _ := s.Put("alice", cli.ImageData{Data: []byte("original"), MimeType: "image/jpeg"})

	ok := s.Replace(id, "alice", cli.ImageData{Data: []byte("rotated!!"), MimeType: "image/jpeg"})
	if !ok {
		t.Fatal("Replace should succeed for a live owned entry")
	}

	// Take must now return the rotated bytes.
	got := s.Take(id, "alice")
	if got == nil {
		t.Fatal("Take after Replace returned nil")
	}
	if string(got.Data) != "rotated!!" {
		t.Errorf("Take after Replace data = %q, want rotated!!", got.Data)
	}
}

func TestUploadStoreReplace_OwnerIsolation(t *testing.T) {
	s := newUploadStore()
	id, _ := s.Put("alice", cli.ImageData{Data: []byte("original"), MimeType: "image/jpeg"})

	if s.Replace(id, "bob", cli.ImageData{Data: []byte("evil"), MimeType: "image/jpeg"}) {
		t.Error("Replace must reject a non-owner")
	}
	if s.Replace("nonexistent", "alice", cli.ImageData{Data: []byte("x"), MimeType: "image/jpeg"}) {
		t.Error("Replace must reject an unknown id")
	}
	// Original bytes must be intact after the rejected replaces.
	if got := s.Take(id, "alice"); got == nil || string(got.Data) != "original" {
		t.Error("rejected Replace must leave the original entry unchanged")
	}
}

// TestUploadStoreReplace_ByteAccounting verifies the per-owner byte counter
// tracks the size delta so a later Put still respects the quota correctly.
func TestUploadStoreReplace_ByteAccounting(t *testing.T) {
	s := newUploadStore()
	id, _ := s.Put("alice", cli.ImageData{Data: make([]byte, 1000), MimeType: "image/jpeg"})

	// Shrink to 100 bytes.
	if !s.Replace(id, "alice", cli.ImageData{Data: make([]byte, 100), MimeType: "image/jpeg"}) {
		t.Fatal("Replace (shrink) should succeed")
	}
	if got := s.ownerBytes["alice"]; got != 100 {
		t.Errorf("ownerBytes after shrink = %d, want 100", got)
	}

	// Grow to 500 bytes.
	if !s.Replace(id, "alice", cli.ImageData{Data: make([]byte, 500), MimeType: "image/jpeg"}) {
		t.Fatal("Replace (grow) should succeed")
	}
	if got := s.ownerBytes["alice"]; got != 500 {
		t.Errorf("ownerBytes after grow = %d, want 500", got)
	}
}

// TestUploadStoreReplace_AfterTakeFailsSafe pins the consumed-entry race the
// review flagged: if a send consumes the upload (Take) while an orient call
// is mid-flight, the late Replace must NOT resurrect a new entry under the
// same id — it must fail safe.
func TestUploadStoreReplace_AfterTakeFailsSafe(t *testing.T) {
	s := newUploadStore()
	id, _ := s.Put("alice", cli.ImageData{Data: []byte("original"), MimeType: "image/jpeg"})

	// Simulate the send path consuming the entry.
	if got := s.Take(id, "alice"); got == nil {
		t.Fatal("Take should consume the entry")
	}
	// A late Replace (orient finished after send) must not re-create it.
	if s.Replace(id, "alice", cli.ImageData{Data: []byte("rotated"), MimeType: "image/jpeg"}) {
		t.Error("Replace after Take must fail safe (entry consumed), not resurrect the upload")
	}
	// And the id must still be gone — no zombie entry.
	if got := s.Take(id, "alice"); got != nil {
		t.Error("Replace must not have re-created the consumed entry")
	}
}

func TestUploadStoreReplace_RejectsOverQuota(t *testing.T) {
	s := newUploadStore()
	id, _ := s.Put("alice", cli.ImageData{Data: make([]byte, 1000), MimeType: "image/jpeg"})

	// A replacement that would exceed the per-owner byte cap must be rejected,
	// and the original entry must survive intact.
	huge := cli.ImageData{Data: make([]byte, maxUploadBytesPerOwner+1), MimeType: "image/jpeg"}
	if s.Replace(id, "alice", huge) {
		t.Error("Replace must reject a payload that exceeds the per-owner byte cap")
	}
	got := s.Take(id, "alice")
	if got == nil || len(got.Data) != 1000 {
		t.Error("rejected over-quota Replace must leave the original bytes intact")
	}
}
