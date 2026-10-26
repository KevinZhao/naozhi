package dispatch

import (
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
// <=10MiB, so the test writes real 8MiB files there.
func TestReadTurnImages_ByteBudget(t *testing.T) {
	// safeImageDirs only permits /tmp. The reply names the literal /tmp
	// path, which on macOS resolves to /private/tmp: the read goes to the
	// resolved path, the rewrite to the written one (#3627).
	dir, err := os.MkdirTemp("/tmp", "naozhi-turn-img-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	const blob = 8 * 1024 * 1024 // 8 MiB per image

	mkImage := func(name string, size int) (path string) {
		path = filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}

	type imgSpec struct {
		name string
		size int
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
			var reply strings.Builder
			reply.WriteString("here are images: ")
			for _, spec := range tc.images {
				reply.WriteString(mkImage(spec.name, spec.size))
				reply.WriteString(" ")
			}

			d, err := NewDispatcher(DispatcherConfig{Turns: testTurns()})
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
	d, err := NewDispatcher(DispatcherConfig{Turns: testTurns()})
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
