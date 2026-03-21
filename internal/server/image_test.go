package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractImagePaths(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "photo.png")
	jpgPath := filepath.Join(dir, "image.jpg")
	os.WriteFile(pngPath, []byte("fake-png"), 0644)
	os.WriteFile(jpgPath, []byte("fake-jpg"), 0644)

	text := "Check " + pngPath + " and also " + jpgPath + " please"
	paths := extractImagePaths(text)

	if len(paths) != 2 {
		t.Fatalf("len(paths) = %d, want 2: %v", len(paths), paths)
	}
	if paths[0] != pngPath {
		t.Errorf("paths[0] = %q, want %q", paths[0], pngPath)
	}
	if paths[1] != jpgPath {
		t.Errorf("paths[1] = %q, want %q", paths[1], jpgPath)
	}
}

func TestExtractImagePaths_NoMatches(t *testing.T) {
	texts := []string{
		"hello world, no images here",
		"some text with relative path image.png but no absolute",
		"https://example.com/pic.png is a URL not a local path",
		"",
	}
	for _, text := range texts {
		paths := extractImagePaths(text)
		if len(paths) != 0 {
			t.Errorf("extractImagePaths(%q) = %v, want empty", text, paths)
		}
	}
}

func TestExtractImagePaths_NonExistent(t *testing.T) {
	text := "look at /tmp/does-not-exist-abc123.png and /nonexistent/photo.jpg"
	paths := extractImagePaths(text)
	if len(paths) != 0 {
		t.Errorf("non-existent paths should be filtered, got %v", paths)
	}
}

func TestExtractImagePaths_Dedup(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "dup.png")
	os.WriteFile(pngPath, []byte("data"), 0644)

	text := pngPath + " and again " + pngPath
	paths := extractImagePaths(text)
	if len(paths) != 1 {
		t.Errorf("duplicate paths should be deduped, got %v", paths)
	}
}

func TestExtractImagePaths_AllExtensions(t *testing.T) {
	dir := t.TempDir()
	exts := []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp"}
	var text string
	for _, ext := range exts {
		p := filepath.Join(dir, "img"+ext)
		os.WriteFile(p, []byte("data"), 0644)
		text += p + " "
	}

	paths := extractImagePaths(text)
	if len(paths) != len(exts) {
		t.Errorf("len(paths) = %d, want %d: %v", len(paths), len(exts), paths)
	}
}

func TestExtractImagePaths_TrailingPunctuation(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "file.png")
	os.WriteFile(pngPath, []byte("data"), 0644)

	// Path followed by punctuation that should be stripped
	text := "See " + pngPath + "."
	paths := extractImagePaths(text)
	if len(paths) != 1 {
		t.Fatalf("len(paths) = %d, want 1", len(paths))
	}
	if paths[0] != pngPath {
		t.Errorf("paths[0] = %q, want %q", paths[0], pngPath)
	}
}

func TestMimeFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/tmp/photo.png", "image/png"},
		{"/tmp/photo.PNG", "image/png"},
		{"/tmp/photo.jpg", "image/jpeg"},
		{"/tmp/photo.JPG", "image/jpeg"},
		{"/tmp/photo.jpeg", "image/jpeg"},
		{"/tmp/photo.gif", "image/gif"},
		{"/tmp/photo.webp", "image/webp"},
		{"/tmp/photo.bmp", "image/bmp"},
		{"/tmp/photo.unknown", "image/png"}, // default fallback
		{"/tmp/noext", "image/png"},         // no extension
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := mimeFromPath(tt.path)
			if got != tt.want {
				t.Errorf("mimeFromPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
