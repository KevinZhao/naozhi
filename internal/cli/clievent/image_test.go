package clievent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestExtractImagePaths(t *testing.T) {
	t.Parallel()
	dir := imageSafeTempDir(t)
	pngPath := filepath.Join(dir, "photo.png")
	jpgPath := filepath.Join(dir, "image.jpg")
	os.WriteFile(pngPath, []byte("fake-png"), 0644)
	os.WriteFile(jpgPath, []byte("fake-jpg"), 0644)

	text := "Check " + pngPath + " and also " + jpgPath + " please"
	paths := ExtractImagePaths(text)

	if len(paths) != 2 {
		t.Fatalf("len(paths) = %d, want 2: %v", len(paths), paths)
	}
	for i, want := range []string{pngPath, jpgPath} {
		if got := paths[i]; got != (ImagePath{Text: want, Path: resolvedPath(t, want)}) {
			t.Errorf("paths[%d] = %+v, want Text %q resolved", i, got, want)
		}
	}
}

// On macOS /tmp is a symlink to /private/tmp: a /tmp path the CLI writes
// resolves outside the literal "/tmp/" prefix and must still be accepted
// (#3627). Text keeps the path as written so the caller can rewrite it.
func TestExtractImagePaths_TmpResolvesThroughSymlink(t *testing.T) {
	t.Parallel()
	dir := imageSafeTempDir(t)
	p := filepath.Join(dir, "x.png")
	if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := ExtractImagePaths("see " + p)
	if len(paths) != 1 {
		t.Fatalf("ExtractImagePaths(%q) = %v, want one path", p, paths)
	}
	if paths[0].Text != p || !isUnderSafeDir(paths[0].Path) || paths[0].Path != resolvedPath(t, p) {
		t.Errorf("paths[0] = %+v, want Text %q and its resolved form as Path", paths[0], p)
	}
}

func TestIsUnderSafeDir(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"/tmp/x.png":          true,
		"/tmp/a/b.png":        true,
		"/tmpfoo/x.png":       false,
		"/tmp":                false,
		"/private/tmpx/a.png": false,
		"/etc/x.png":          false,
	} {
		if got := isUnderSafeDir(path); got != want {
			t.Errorf("isUnderSafeDir(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestResolveSafeDirs(t *testing.T) {
	t.Parallel()
	dir := imageSafeTempDir(t)
	link := filepath.Join(dir, "link")
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got := resolveSafeDirs([]string{link + "/", "/nonexistent-naozhi-dir/"})
	want := []string{link + "/", "/nonexistent-naozhi-dir/", resolvedPath(t, target) + "/"}
	if !slices.Equal(got, want) {
		t.Errorf("resolveSafeDirs = %q, want %q", got, want)
	}
}

func TestExtractImagePaths_NoMatches(t *testing.T) {
	t.Parallel()
	texts := []string{
		"hello world, no images here",
		"some text with relative path image.png but no absolute",
		"https://example.com/pic.png is a URL not a local path",
		"",
	}
	for _, text := range texts {
		paths := ExtractImagePaths(text)
		if len(paths) != 0 {
			t.Errorf("ExtractImagePaths(%q) = %v, want empty", text, paths)
		}
	}
}

func TestExtractImagePaths_NonExistent(t *testing.T) {
	t.Parallel()
	text := "look at /tmp/does-not-exist-abc123.png and /nonexistent/photo.jpg"
	paths := ExtractImagePaths(text)
	if len(paths) != 0 {
		t.Errorf("non-existent paths should be filtered, got %v", paths)
	}
}

func TestExtractImagePaths_Dedup(t *testing.T) {
	t.Parallel()
	dir := imageSafeTempDir(t)
	pngPath := filepath.Join(dir, "dup.png")
	os.WriteFile(pngPath, []byte("data"), 0644)

	text := pngPath + " and again " + pngPath
	paths := ExtractImagePaths(text)
	if len(paths) != 1 {
		t.Errorf("duplicate paths should be deduped, got %v", paths)
	}
}

func TestExtractImagePaths_AllExtensions(t *testing.T) {
	t.Parallel()
	dir := imageSafeTempDir(t)
	exts := []string{".png", ".jpg", ".jpeg", ".gif", ".webp"}
	var text string
	for _, ext := range exts {
		p := filepath.Join(dir, "img"+ext)
		os.WriteFile(p, []byte("data"), 0644)
		text += p + " "
	}
	// The model rejects bmp, so a .bmp path is left as text (#3624).
	bmp := filepath.Join(dir, "img.bmp")
	os.WriteFile(bmp, []byte("data"), 0644)
	text += bmp

	paths := ExtractImagePaths(text)
	if len(paths) != len(exts) {
		t.Errorf("len(paths) = %d, want %d: %v", len(paths), len(exts), paths)
	}
}

func TestExtractImagePaths_TrailingPunctuation(t *testing.T) {
	t.Parallel()
	dir := imageSafeTempDir(t)
	pngPath := filepath.Join(dir, "file.png")
	os.WriteFile(pngPath, []byte("data"), 0644)

	// Path followed by punctuation that should be stripped
	text := "See " + pngPath + "."
	paths := ExtractImagePaths(text)
	if len(paths) != 1 {
		t.Fatalf("len(paths) = %d, want 1", len(paths))
	}
	if paths[0].Text != pngPath {
		t.Errorf("paths[0].Text = %q, want %q", paths[0].Text, pngPath)
	}
}

func TestMimeFromPath(t *testing.T) {
	t.Parallel()
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
		{"/tmp/photo.unknown", "image/png"}, // default fallback
		{"/tmp/noext", "image/png"},         // no extension
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			got := MimeFromPath(tt.path)
			if got != tt.want {
				t.Errorf("MimeFromPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
