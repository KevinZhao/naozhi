package cli

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// solidJPEG builds a w×h all-white JPEG. Content is irrelevant for the
// dimension-math assertions; we only need a decodable image of a given size.
func solidJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func decodeDims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestTargetScale(t *testing.T) {
	tests := []struct {
		name       string
		w, h       int
		wantScaled bool // true if scale < 1 expected
	}{
		{"within both limits", 800, 600, false},
		{"exact area knee ok", 1000, 1000, false}, // 1.0 MP < 1.15 MP
		{"gaokao 1200x1600 area overflow", 1200, 1600, true},
		{"long edge overflow only", 2000, 400, true},
		{"tiny", 10, 10, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := targetScale(tc.w, tc.h)
			if tc.wantScaled && s >= 1.0 {
				t.Fatalf("expected downscale for %dx%d, got scale=%v", tc.w, tc.h, s)
			}
			if !tc.wantScaled && s < downscaleSkipRatio {
				t.Fatalf("expected no downscale for %dx%d, got scale=%v", tc.w, tc.h, s)
			}
			if tc.wantScaled {
				dw := float64(tc.w) * s
				dh := float64(tc.h) * s
				long := dw
				if dh > long {
					long = dh
				}
				// Allow 1px rounding slack on each check.
				if long > float64(maxVisionLongEdge)+1 {
					t.Fatalf("long edge %v still exceeds %d", long, maxVisionLongEdge)
				}
				if dw*dh > float64(maxVisionPixels)*1.01 {
					t.Fatalf("area %v still exceeds %d", dw*dh, maxVisionPixels)
				}
			}
		})
	}
}

func TestDownscaleForVision_ShrinksOversized(t *testing.T) {
	// The exact gaokao case: 1200×1600 (1.92 MP) -> under 1.15 MP.
	src := solidJPEG(t, 1200, 1600)
	out, mime, changed := downscaleForVision(src)
	if !changed {
		t.Fatal("expected downscale to occur")
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime=%q, want image/jpeg", mime)
	}
	w, h := decodeDims(t, out)
	if int64(w)*int64(h) > int64(float64(maxVisionPixels)*1.02) {
		t.Fatalf("output %dx%d = %d px still over %d", w, h, w*h, maxVisionPixels)
	}
	if len(out) >= len(src) {
		t.Fatalf("expected smaller payload: out=%d src=%d", len(out), len(src))
	}
	// Aspect ratio preserved (within 1%).
	srcAR := 1200.0 / 1600.0
	outAR := float64(w) / float64(h)
	if d := srcAR - outAR; d > 0.01 || d < -0.01 {
		t.Fatalf("aspect ratio drift: src=%v out=%v", srcAR, outAR)
	}
}

func TestDownscaleForVision_PassthroughWhenSmall(t *testing.T) {
	src := solidJPEG(t, 800, 600) // 0.48 MP, under both limits
	out, mime, changed := downscaleForVision(src)
	if changed {
		t.Fatalf("expected passthrough for small image, got changed=true (mime=%q out=%d)", mime, len(out))
	}
	if out != nil {
		t.Fatal("expected nil out on passthrough")
	}
}

func TestDownscaleForVision_PassthroughWhenUndecodable(t *testing.T) {
	_, _, changed := downscaleForVision([]byte("not an image"))
	if changed {
		t.Fatal("expected passthrough for undecodable bytes")
	}
}

func TestDownscaleImagesForVision_Immutable(t *testing.T) {
	src := solidJPEG(t, 1200, 1600)
	in := []Attachment{
		{Kind: KindImageInline, Data: src, MimeType: "image/jpeg"},
	}
	origData := in[0].Data
	out := downscaleImagesForVision(in)

	// Input slice element must be untouched (immutability contract).
	if !bytes.Equal(in[0].Data, origData) {
		t.Fatal("input Attachment.Data was mutated")
	}
	if len(out) != 1 {
		t.Fatalf("len(out)=%d, want 1", len(out))
	}
	if bytes.Equal(out[0].Data, origData) {
		t.Fatal("expected downscaled bytes to differ from original")
	}
	if len(out[0].Data) >= len(origData) {
		t.Fatalf("expected smaller output: out=%d orig=%d", len(out[0].Data), len(origData))
	}
}

func TestDownscaleImagesForVision_SkipsFileRef(t *testing.T) {
	// A file_ref carrying oversized raster bytes in Data is the only case that
	// actually exercises the Kind guard: with nil/empty Data the decode fails
	// anyway and every attachment takes the same pass-through path, so the
	// guard could be deleted without the test noticing. Here the bytes WOULD
	// be downscaled if Kind were ignored, so removing
	// `img.Kind == KindFileRef` makes this fail.
	oversized := solidJPEG(t, 1200, 1600)
	in := []Attachment{
		{Kind: KindFileRef, WorkspacePath: "docs/x.pdf", MimeType: "application/pdf", Data: oversized},
		{Kind: KindFileRef, WorkspacePath: "docs/y.pdf", MimeType: "application/pdf"},
		{Kind: KindImageInline, Data: nil, MimeType: "image/png"}, // empty inline
	}
	out := downscaleImagesForVision(in)
	if !bytes.Equal(out[0].Data, oversized) {
		t.Errorf("file_ref bytes were downscaled: in=%d out=%d — the Kind guard is not being applied",
			len(oversized), len(out[0].Data))
	}
	if out[0].MimeType != "application/pdf" {
		t.Errorf("file_ref MimeType rewritten to %q", out[0].MimeType)
	}
	if out[1].Kind != KindFileRef || out[1].WorkspacePath != "docs/y.pdf" {
		t.Error("data-less file_ref attachment was altered")
	}
	if out[2].Data != nil {
		t.Error("empty inline attachment was altered")
	}
}

// TestDownscaleForVision_TransparentFlattensToWhite guards the JPEG-has-no-alpha
// flattening: a transparent PNG must composite over WHITE, not the RGBA zero
// value (transparent black), so document screenshots don't come out darkened.
func TestDownscaleForVision_TransparentFlattensToWhite(t *testing.T) {
	// Fully transparent 1600×1600 PNG (area over the 1.15 MP knee).
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1600))
	for y := 0; y < 1600; y++ {
		for x := 0; x < 1600; x++ {
			img.SetRGBA(x, y, color.RGBA{0, 0, 0, 0}) // transparent
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	out, _, changed := downscaleForVision(buf.Bytes())
	if !changed {
		t.Fatal("expected downscale for 1600x1600")
	}
	dec, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode out: %v", err)
	}
	r, g, b, _ := dec.At(50, 50).RGBA()
	// Transparent-over-white → white. JPEG is lossy, so allow slack.
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("transparent region should flatten to white, got R=%d G=%d B=%d", r>>8, g>>8, b>>8)
	}
}

func TestDownscaleImagesForVision_Empty(t *testing.T) {
	if out := downscaleImagesForVision(nil); out != nil {
		t.Fatal("nil input should return nil")
	}
	if out := downscaleImagesForVision([]Attachment{}); len(out) != 0 {
		t.Fatal("empty input should return empty")
	}
}
