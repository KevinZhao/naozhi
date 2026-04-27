package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestUploadStoreOwnership(t *testing.T) {
	s := newUploadStore()
	img := cli.ImageData{Data: []byte("fake"), MimeType: "image/png"}

	id, err := s.Put("alice", img)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Wrong owner must not retrieve the entry.
	if got := s.Take(id, "bob"); got != nil {
		t.Error("Take with wrong owner should return nil, got non-nil")
	}

	// Correct owner retrieves the entry exactly once.
	got := s.Take(id, "alice")
	if got == nil {
		t.Fatal("Take with correct owner returned nil")
	}

	// Entry is consumed; second Take by same owner returns nil.
	if s.Take(id, "alice") != nil {
		t.Error("second Take should return nil (already consumed)")
	}
}

func TestUploadStoreNotFound(t *testing.T) {
	s := newUploadStore()
	if got := s.Take("nonexistent", "alice"); got != nil {
		t.Error("Take on missing id should return nil")
	}
}

// TestUploadStoreTakeAll_AllOrNothingOnMissingID locks R37-CONCUR4: when
// a batch of fids is handed to TakeAll and one of them does not exist,
// NO entry is consumed — the caller's earlier-valid fids must still be
// retrievable on a subsequent TakeAll (perhaps after the user re-uploads
// the missing image). The old per-fid loop in handleSend would consume
// the first N-1 fids before hitting the bad fid, losing those images.
func TestUploadStoreTakeAll_AllOrNothingOnMissingID(t *testing.T) {
	s := newUploadStore()
	img1 := cli.ImageData{Data: []byte("one"), MimeType: "image/png"}
	img2 := cli.ImageData{Data: []byte("two"), MimeType: "image/jpeg"}

	id1, err := s.Put("alice", img1)
	if err != nil {
		t.Fatalf("Put img1: %v", err)
	}
	id2, err := s.Put("alice", img2)
	if err != nil {
		t.Fatalf("Put img2: %v", err)
	}

	// Batch contains two real ids + one bogus id. TakeAll must return
	// an error and leave the two real entries untouched.
	taken, err := s.TakeAll([]string{id1, "nonexistent", id2}, "alice")
	if err == nil {
		t.Fatalf("TakeAll with missing id: want error, got %v", taken)
	}
	if taken != nil {
		t.Errorf("TakeAll error path returned non-nil slice: %v", taken)
	}

	// Both real entries must still be intact; verify by taking them
	// individually afterwards.
	if got := s.Take(id1, "alice"); got == nil {
		t.Error("id1 should still be retrievable after failed TakeAll")
	}
	if got := s.Take(id2, "alice"); got == nil {
		t.Error("id2 should still be retrievable after failed TakeAll")
	}
}

// TestUploadStoreTakeAll_AllOrNothingOnOwnerMismatch covers the "foreign
// owner in the middle of the batch" case — the owner mismatch must not
// silently consume the caller's valid entries before the check fires.
func TestUploadStoreTakeAll_AllOrNothingOnOwnerMismatch(t *testing.T) {
	s := newUploadStore()
	alice1, _ := s.Put("alice", cli.ImageData{Data: []byte("a1"), MimeType: "image/png"})
	bob, _ := s.Put("bob", cli.ImageData{Data: []byte("b"), MimeType: "image/png"})
	alice2, _ := s.Put("alice", cli.ImageData{Data: []byte("a2"), MimeType: "image/png"})

	taken, err := s.TakeAll([]string{alice1, bob, alice2}, "alice")
	if err == nil {
		t.Fatalf("TakeAll with foreign-owned id: want error, got %v", taken)
	}
	// alice's entries must both survive.
	if got := s.Take(alice1, "alice"); got == nil {
		t.Error("alice1 should survive failed TakeAll")
	}
	if got := s.Take(alice2, "alice"); got == nil {
		t.Error("alice2 should survive failed TakeAll")
	}
	// bob's entry must survive too — the owner-mismatch path must not
	// delete it either (no data destruction on foreign id even though
	// the caller doesn't own it).
	if got := s.Take(bob, "bob"); got == nil {
		t.Error("bob's entry should survive failed TakeAll by alice")
	}
}

// TestUploadStoreTakeAll_HappyPathConsumesAllInOrder verifies the
// success case: all ids resolve, images come back in the input order,
// and every entry is removed.
func TestUploadStoreTakeAll_HappyPathConsumesAllInOrder(t *testing.T) {
	s := newUploadStore()
	img1 := cli.ImageData{Data: []byte("one"), MimeType: "image/png"}
	img2 := cli.ImageData{Data: []byte("two"), MimeType: "image/jpeg"}
	img3 := cli.ImageData{Data: []byte("three"), MimeType: "image/gif"}

	id1, _ := s.Put("alice", img1)
	id2, _ := s.Put("alice", img2)
	id3, _ := s.Put("alice", img3)

	taken, err := s.TakeAll([]string{id1, id2, id3}, "alice")
	if err != nil {
		t.Fatalf("TakeAll happy path: %v", err)
	}
	if len(taken) != 3 {
		t.Fatalf("taken length = %d, want 3", len(taken))
	}
	if string(taken[0].Data) != "one" || string(taken[1].Data) != "two" || string(taken[2].Data) != "three" {
		t.Errorf("order broken: got %q %q %q", taken[0].Data, taken[1].Data, taken[2].Data)
	}

	// All entries consumed — a second TakeAll must fail.
	if _, err := s.TakeAll([]string{id1}, "alice"); err == nil {
		t.Error("second TakeAll after full consume should error")
	}
}

// TestUploadStoreTakeAll_EmptySliceReturnsNilNoErr documents the
// trivial case so callers can pass a nil/empty slice unconditionally
// without special-casing it.
func TestUploadStoreTakeAll_EmptySliceReturnsNilNoErr(t *testing.T) {
	s := newUploadStore()
	taken, err := s.TakeAll(nil, "alice")
	if err != nil || taken != nil {
		t.Errorf("TakeAll(nil) = (%v, %v), want (nil, nil)", taken, err)
	}
	taken, err = s.TakeAll([]string{}, "alice")
	if err != nil || taken != nil {
		t.Errorf("TakeAll([]) = (%v, %v), want (nil, nil)", taken, err)
	}
}
