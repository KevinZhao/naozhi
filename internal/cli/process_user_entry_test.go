package cli

import (
	"bytes"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// buildUserEntry populates EventEntry.ImagePaths from Attachment.WorkspacePath
// index-aligned with EventEntry.Images. This is the contract the
// dashboard lightbox relies on: clicking the i-th thumbnail navigates to
// /api/sessions/attachment?...&path=ImagePaths[i].
func TestBuildUserEntry_PopulatesImagePaths(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := png.Encode(&buf, newSolidImage(8, 8, color.RGBA{255, 0, 0, 255})); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	pngBytes := buf.Bytes()
	imgs := []ImageData{
		{
			Data:          pngBytes,
			MimeType:      "image/png",
			WorkspacePath: ".naozhi/attachments/2026-05-07/aaaa.png",
		},
		{
			Data:          pngBytes,
			MimeType:      "image/png",
			WorkspacePath: ".naozhi/attachments/2026-05-07/bbbb.png",
		},
	}
	entry := buildUserEntry("look at these", imgs)

	if len(entry.Images) != 2 {
		t.Fatalf("Images len=%d want 2", len(entry.Images))
	}
	if len(entry.ImagePaths) != 2 {
		t.Fatalf("ImagePaths len=%d want 2", len(entry.ImagePaths))
	}
	for i, p := range entry.ImagePaths {
		if !strings.HasPrefix(p, ".naozhi/attachments/") {
			t.Errorf("ImagePaths[%d]=%q not rooted", i, p)
		}
	}
	// Index alignment: thumb[i] must correspond to ImagePaths[i].
	if entry.ImagePaths[0] != ".naozhi/attachments/2026-05-07/aaaa.png" {
		t.Errorf("ImagePaths[0]=%q", entry.ImagePaths[0])
	}
	if entry.ImagePaths[1] != ".naozhi/attachments/2026-05-07/bbbb.png" {
		t.Errorf("ImagePaths[1]=%q", entry.ImagePaths[1])
	}
}

// Legacy path: attachments with no WorkspacePath (e.g. IM adapters that
// never persist) must leave ImagePaths unset so the dashboard falls back
// to the embedded thumbnail data URI.
func TestBuildUserEntry_NoPathsWhenUnpersisted(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := png.Encode(&buf, newSolidImage(8, 8, color.RGBA{0, 255, 0, 255})); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	imgs := []ImageData{{Data: buf.Bytes(), MimeType: "image/png"}}
	entry := buildUserEntry("hi", imgs)
	if len(entry.Images) != 1 {
		t.Fatalf("Images len=%d want 1", len(entry.Images))
	}
	if entry.ImagePaths != nil {
		t.Errorf("ImagePaths should be nil when no attachment has a path, got %v", entry.ImagePaths)
	}
}

// When MakeThumbnail drops one input (undecodable bytes) the surviving
// thumbs and ImagePaths MUST remain index-aligned — a mismatched pair
// would send the lightbox to the wrong file.
func TestBuildUserEntry_AlignmentSurvivesDrop(t *testing.T) {
	t.Parallel()
	var ok bytes.Buffer
	if err := png.Encode(&ok, newSolidImage(8, 8, color.RGBA{0, 0, 255, 255})); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	imgs := []ImageData{
		{Data: []byte("not-an-image"), MimeType: "image/png",
			WorkspacePath: ".naozhi/attachments/x/undecodable.png"},
		{Data: ok.Bytes(), MimeType: "image/png",
			WorkspacePath: ".naozhi/attachments/x/valid.png"},
	}
	entry := buildUserEntry("mixed", imgs)

	// Undecodable one is dropped from Images — ImagePaths must drop in
	// lock-step (not just "the last N paths").
	if len(entry.Images) != 1 {
		t.Fatalf("Images len=%d want 1 (undecodable should drop)", len(entry.Images))
	}
	if len(entry.ImagePaths) != 1 {
		t.Fatalf("ImagePaths len=%d want 1", len(entry.ImagePaths))
	}
	if entry.ImagePaths[0] != ".naozhi/attachments/x/valid.png" {
		t.Errorf("ImagePaths[0]=%q want valid.png — alignment broken",
			entry.ImagePaths[0])
	}
}
