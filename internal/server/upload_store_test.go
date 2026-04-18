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
