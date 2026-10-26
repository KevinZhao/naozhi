package cli

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// makeTestPNG renders a tiny solid-colour PNG so MakeThumbnail has real
// data to decode. 8x8 keeps decode cheap so we don't slow down the
// many-image stress case below.
func makeTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

// Worker-pool path must produce identical output to the pre-fix
// "spawn-per-image" loop: thumbnails are index-aligned with the input
// images slice. Drift here would scramble the dashboard's image order.
func TestBuildUserEntry_OrderPreserved(t *testing.T) {
	pngData := makeTestPNG(t)
	const N = thumbnailWorkerCap*2 + 1 // cross the cap boundary
	images := make([]clievent.Attachment, N)
	for i := range images {
		images[i] = clievent.Attachment{
			Data:          pngData,
			WorkspacePath: "/path/" + strconv.Itoa(i),
		}
	}

	entry := buildUserEntry("hi", images, "")
	if len(entry.ImagePaths) != len(entry.Images) {
		t.Fatalf("ImagePaths length %d != Images length %d",
			len(entry.ImagePaths), len(entry.Images))
	}
	// Each surviving thumbnail's path must match its source path —
	// post-fix the worker pool reorders work, so the index alignment
	// guarantee is the contract under test.
	for i, p := range entry.ImagePaths {
		want := "/path/" + strconv.Itoa(i)
		if p != want {
			t.Errorf("ImagePaths[%d] = %q, want %q", i, p, want)
		}
	}
}
