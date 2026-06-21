package dispatch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadTurnImages_ByteBudget pins #2196: the per-turn outbound image read
// loop enforces maxTurnImageBytes across all attachments. Over-budget images
// are skipped but their paths are STILL rewritten to "[图片]" so the
// user-visible text is identical regardless of attachment outcome; an
// under-budget image attaches normally.
//
// ExtractImagePaths only returns paths that exist on disk under /tmp and are
// <=10MiB, so the test creates small real placeholder files there and keys
// the fakeImageReader (which drives the byte budget) by those same paths.
func TestReadTurnImages_ByteBudget(t *testing.T) {
	dir := t.TempDir()
	// safeImageDirs only permits /tmp; t.TempDir() is under /tmp on Linux CI,
	// but be defensive and skip if it ever isn't.
	if !strings.HasPrefix(dir, "/tmp/") {
		t.Skipf("TempDir %q not under /tmp; ExtractImagePaths safe-dir gate would reject", dir)
	}

	const blob = 8 * 1024 * 1024 // 8 MiB per fake image

	mkImage := func(name string) (path string) {
		path = filepath.Join(dir, name)
		// Real on-disk file must exist + be small (<=10MiB) for ExtractImagePaths.
		if err := os.WriteFile(path, []byte("placeholder"), 0o600); err != nil {
			t.Fatalf("write placeholder %s: %v", path, err)
		}
		return path
	}

	type imgSpec struct {
		name      string
		fakeBytes int
	}

	cases := []struct {
		name           string
		images         []imgSpec
		wantAttached   int // number of images attached
		wantTotalBytes int // exact summed bytes of attached images
	}{
		{
			name:           "single under budget attaches",
			images:         []imgSpec{{"a.png", blob}},
			wantAttached:   1,
			wantTotalBytes: blob,
		},
		{
			name: "over-budget images skipped but text still rewritten",
			// 4 x 8MiB = 32MiB > 20MiB budget: only the first two (16MiB) fit.
			images: []imgSpec{
				{"a.png", blob},
				{"b.png", blob},
				{"c.png", blob},
				{"d.png", blob},
			},
			wantAttached:   2,
			wantTotalBytes: 2 * blob,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := make(map[string][]byte)
			var reply strings.Builder
			reply.WriteString("here are images: ")
			for _, spec := range tc.images {
				p := mkImage(spec.name)
				files[p] = make([]byte, spec.fakeBytes)
				reply.WriteString(p)
				reply.WriteString(" ")
			}

			d, err := NewDispatcher(DispatcherConfig{
				AllowMissingSender: true,
				ImageReader:        &fakeImageReader{files: files},
			})
			if err != nil {
				t.Fatalf("NewDispatcher: %v", err)
			}

			outImages, outText := d.readTurnImages(reply.String())

			// (1) total attached bytes within budget
			total := 0
			for _, img := range outImages {
				total += len(img.Data)
			}
			if total > maxTurnImageBytes {
				t.Errorf("attached %d bytes exceeds budget %d", total, maxTurnImageBytes)
			}
			if total != tc.wantTotalBytes {
				t.Errorf("attached bytes = %d, want %d", total, tc.wantTotalBytes)
			}
			if len(outImages) != tc.wantAttached {
				t.Errorf("attached count = %d, want %d", len(outImages), tc.wantAttached)
			}

			// (2)/(3) every path is rewritten to "[图片]" regardless of
			// attachment outcome — no raw /tmp path leaks into user text.
			if strings.Contains(outText, dir) {
				t.Errorf("output text still contains raw path under %q: %q", dir, outText)
			}
			if got := strings.Count(outText, "[图片]"); got != len(tc.images) {
				t.Errorf("rewrote %d paths to [图片], want %d; text=%q", got, len(tc.images), outText)
			}
		})
	}
}

// TestReadTurnImages_NoPaths returns the text unchanged with no attachments.
func TestReadTurnImages_NoPaths(t *testing.T) {
	d, err := NewDispatcher(DispatcherConfig{
		AllowMissingSender: true,
		ImageReader:        &fakeImageReader{},
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	const text = "just a plain reply, no images here"
	imgs, out := d.readTurnImages(text)
	if imgs != nil {
		t.Errorf("expected nil images, got %d", len(imgs))
	}
	if out != text {
		t.Errorf("text mutated: got %q, want %q", out, text)
	}
}

// TestImageReader_DefaultIsOsImageReader pins that NewDispatcher installs
// osImageReader{} when DispatcherConfig.ImageReader is left nil, so
// existing production wiring keeps reading attachment paths off disk
// without an explicit field. R245-ARCH-33 (#884).
func TestImageReader_DefaultIsOsImageReader(t *testing.T) {
	d, err := NewDispatcher(DispatcherConfig{
		AllowMissingSender: true,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if d.imageReader == nil {
		t.Fatalf("NewDispatcher left d.imageReader nil; default osImageReader{} must be installed")
	}
	if _, ok := d.imageReader.(osImageReader); !ok {
		t.Errorf("default ImageReader = %T, want dispatch.osImageReader", d.imageReader)
	}
}

// TestImageReader_OverrideHonoured pins that DispatcherConfig.ImageReader,
// when non-nil, replaces the default — the seam tests need to drive the
// read-success / read-failure branches without writing to /tmp.
func TestImageReader_OverrideHonoured(t *testing.T) {
	fake := &fakeImageReader{}
	d, err := NewDispatcher(DispatcherConfig{
		AllowMissingSender: true,
		ImageReader:        fake,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if d.imageReader != ImageReader(fake) {
		t.Fatalf("DispatcherConfig.ImageReader override ignored; got %T", d.imageReader)
	}
}

// TestOsImageReader_ReadFileMissingReturnsError pins the sole production
// guarantee callers in sendAndReply rely on: a missing file produces a
// non-nil error so the path is rewritten to "[图片]" without a partial
// attachment. We do not assert the exact error type — that is os.ReadFile's
// contract — only that one is returned.
func TestOsImageReader_ReadFileMissingReturnsError(t *testing.T) {
	r := osImageReader{}
	_, err := r.ReadFile("/nonexistent/dispatch-imagereader-test.png")
	if err == nil {
		t.Fatalf("osImageReader.ReadFile on missing path returned nil error")
	}
}

// fakeImageReader records calls and returns canned results. Useful for
// tests asserting that sendAndReply routes attachment reads through the
// seam rather than the global os.ReadFile.
type fakeImageReader struct {
	files map[string][]byte
	err   error
}

func (f *fakeImageReader) ReadFile(path string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.files == nil {
		return nil, errors.New("fakeImageReader: no files configured")
	}
	data, ok := f.files[path]
	if !ok {
		return nil, errors.New("fakeImageReader: not found")
	}
	return data, nil
}
