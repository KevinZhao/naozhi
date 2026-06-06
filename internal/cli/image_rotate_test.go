package cli

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"
)

// TestRotateJPEG_UsesOrientSemNotThumbSem guards R20260606-GO-2 (#1844):
// auto-orient must serialise on its own orientSem and never consume a
// thumbSem slot, so a burst of orient decodes cannot starve the
// user-visible MakeThumbnail path.
func TestRotateJPEG_UsesOrientSemNotThumbSem(t *testing.T) {
	// Saturate thumbSem so that if RotateJPEG (wrongly) tried to acquire a
	// thumbSem token it would block forever; a correct implementation uses
	// orientSem and proceeds.
	for i := 0; i < cap(thumbSem); i++ {
		thumbSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(thumbSem); i++ {
			<-thumbSem
		}
	}()

	src := makeTestJPEG(t, 8, 4, color.RGBA{A: 255})
	done := make(chan bool, 1)
	go func() {
		_, ok := RotateJPEG(src, 90)
		done <- ok
	}()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("RotateJPEG failed while thumbSem was saturated")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RotateJPEG blocked while thumbSem was saturated — it is sharing thumbSem instead of using orientSem")
	}
}

// TestOrientSemIsDistinctFromThumbSem asserts the two semaphores are separate
// channels with the orient cap kept below the thumbnail cap.
func TestOrientSemIsDistinctFromThumbSem(t *testing.T) {
	// thumbSem is chan struct{} and orientSem is chan struct{}; comparing the
	// channel values checks they refer to different underlying channels.
	if (chan struct{})(orientSem) == (chan struct{})(thumbSem) {
		t.Fatal("orientSem must be a distinct channel from thumbSem")
	}
	if cap(orientSem) <= 0 {
		t.Fatalf("orientSem cap must be positive, got %d", cap(orientSem))
	}
	if cap(orientSem) >= cap(thumbSem) {
		t.Errorf("orientSem cap (%d) should stay below thumbSem cap (%d) so orient cannot dominate decode capacity",
			cap(orientSem), cap(thumbSem))
	}
}

// makeTestJPEG builds a w×h image where a 2×2 block at the top-left corner
// is painted a marker color and everything else is white, then JPEG-encodes
// it. The marker lets a test assert WHERE the top-left of the source landed
// after rotation. Returns the encoded bytes.
func makeTestJPEG(t *testing.T, w, h int, marker color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	// Paint a distinct marker at the top-left corner (0,0).
	img.SetRGBA(0, 0, marker)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

// isDark reports whether a pixel is clearly non-white (the marker survives
// JPEG round-trip as "much darker than white" even with quality loss).
func isDark(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	// White is ~0xffff on each channel; the marker (black/red) drops at
	// least one channel well below half. Use a generous threshold so JPEG
	// ringing around the 2×2 block doesn't cause a false negative.
	return r < 0x8000 || g < 0x8000 || b < 0x8000
}

func decodeRGBA(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode rotated jpeg: %v", err)
	}
	return img
}

func TestRotateJPEG_DegreesZeroIsPassthrough(t *testing.T) {
	src := makeTestJPEG(t, 8, 4, color.RGBA{A: 255}) // black marker
	out, ok := RotateJPEG(src, 0)
	if !ok {
		t.Fatal("RotateJPEG(0) should succeed")
	}
	if !bytes.Equal(out, src) {
		t.Error("RotateJPEG(0) must return the input bytes unchanged (passthrough)")
	}
}

func TestRotateJPEG_RejectsInvalidDegrees(t *testing.T) {
	src := makeTestJPEG(t, 8, 4, color.RGBA{A: 255})
	for _, deg := range []int{1, 45, 89, 91, 360, -90, 271} {
		if out, ok := RotateJPEG(src, deg); ok || out != nil {
			t.Errorf("RotateJPEG(%d) must fail with (nil,false), got ok=%v len=%d", deg, ok, len(out))
		}
	}
}

func TestRotateJPEG_RejectsGarbage(t *testing.T) {
	if out, ok := RotateJPEG([]byte("not an image"), 90); ok || out != nil {
		t.Error("RotateJPEG on garbage bytes must fail safe with (nil,false)")
	}
}

// TestRotateJPEG_DimensionsSwap verifies 90/270 swap W/H and 180 preserves.
func TestRotateJPEG_DimensionsSwap(t *testing.T) {
	src := makeTestJPEG(t, 8, 4, color.RGBA{A: 255})
	cases := []struct {
		deg   int
		wantW int
		wantH int
	}{
		{90, 4, 8},
		{180, 8, 4},
		{270, 4, 8},
	}
	for _, c := range cases {
		out, ok := RotateJPEG(src, c.deg)
		if !ok {
			t.Fatalf("RotateJPEG(%d) failed", c.deg)
		}
		img := decodeRGBA(t, out)
		b := img.Bounds()
		if b.Dx() != c.wantW || b.Dy() != c.wantH {
			t.Errorf("RotateJPEG(%d) dims = %dx%d, want %dx%d", c.deg, b.Dx(), b.Dy(), c.wantW, c.wantH)
		}
	}
}

// TestRotateJPEG_EdgeDimensions exercises odd, 1×N, N×1, and square images
// through every rotation to catch off-by-one / index-out-of-range in the
// coordinate maps (math review flagged these as the classic break points).
func TestRotateJPEG_EdgeDimensions(t *testing.T) {
	dims := [][2]int{{1, 1}, {1, 7}, {7, 1}, {3, 5}, {5, 3}, {4, 4}, {2, 1}, {1, 2}}
	for _, d := range dims {
		w, h := d[0], d[1]
		src := makeTestJPEG(t, w, h, color.RGBA{A: 255})
		for _, deg := range []int{90, 180, 270} {
			out, ok := RotateJPEG(src, deg)
			if !ok {
				t.Errorf("RotateJPEG(%dx%d, %d) failed — likely an index panic recovered", w, h, deg)
				continue
			}
			img := decodeRGBA(t, out)
			wantW, wantH := w, h
			if deg == 90 || deg == 270 {
				wantW, wantH = h, w
			}
			if b := img.Bounds(); b.Dx() != wantW || b.Dy() != wantH {
				t.Errorf("RotateJPEG(%dx%d, %d) dims = %dx%d, want %dx%d", w, h, deg, b.Dx(), b.Dy(), wantW, wantH)
			}
		}
	}
}

// TestRotateJPEG_FourQuarterTurnsRoundTrip rotates 90° four times and expects
// to land back at the original dimensions (a coarse invariant that a wrong
// per-step swap would violate).
func TestRotateJPEG_FourQuarterTurnsRoundTrip(t *testing.T) {
	cur := makeTestJPEG(t, 6, 4, color.RGBA{A: 255})
	for i := 0; i < 4; i++ {
		out, ok := RotateJPEG(cur, 90)
		if !ok {
			t.Fatalf("turn %d failed", i)
		}
		cur = out
	}
	img := decodeRGBA(t, cur)
	if b := img.Bounds(); b.Dx() != 6 || b.Dy() != 4 {
		t.Errorf("after 4×90° dims = %dx%d, want 6x4", b.Dx(), b.Dy())
	}
}

// TestRotateJPEG_MarkerLandsCorrectly verifies the top-left marker ends up
// at the geometrically correct corner for each clockwise rotation. This is
// the real correctness check — dimensions alone don't prove the rotation
// direction is right.
//
// Source marker at (0,0) of an w×h image. Under a CLOCKWISE rotation:
//   - 90°  CW: top-left -> top-right.   dest dims h×w, marker at (h-1, 0)
//   - 180° CW: top-left -> bottom-right. dest dims w×h, marker at (w-1, h-1)
//   - 270° CW: top-left -> bottom-left.  dest dims h×w, marker at (0, w-1)
func TestRotateJPEG_MarkerLandsCorrectly(t *testing.T) {
	const w, h = 8, 4
	src := makeTestJPEG(t, w, h, color.RGBA{A: 255}) // black marker at (0,0)

	cases := []struct {
		deg     int
		markerX int
		markerY int
	}{
		{90, h - 1, 0},
		{180, w - 1, h - 1},
		{270, 0, w - 1},
	}
	for _, c := range cases {
		out, ok := RotateJPEG(src, c.deg)
		if !ok {
			t.Fatalf("RotateJPEG(%d) failed", c.deg)
		}
		img := decodeRGBA(t, out)
		if !isDark(img.At(c.markerX, c.markerY)) {
			t.Errorf("RotateJPEG(%d): expected marker at (%d,%d) but pixel is light; rotation direction likely wrong",
				c.deg, c.markerX, c.markerY)
		}
	}
}
