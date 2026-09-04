package cli

import (
	"bytes"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log/slog"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// orientWorkerCap bounds concurrent auto-orient decodes. Orient is a
// user-triggered, rate-limited path (POST /api/sessions/orient, 10/min) with
// one decode per request, so 2 suffices. Deliberately not thumbnailWorkerCap,
// which sizes the thumbnail worker pool in process_send.go.
const orientWorkerCap = 2

// orientSem is separate from thumbSem so auto-orient and multi-image
// thumbnail generation cannot starve each other.
var orientSem = make(chan struct{}, orientWorkerCap)

// RotateJPEG decodes raw image bytes, rotates the pixels clockwise by
// `degCW` (must be 90, 180, or 270), and re-encodes to JPEG. degCW==0
// returns the input unchanged. Used by auto-orient to bake the rotation into
// EXIF-less images so every downstream consumer sees them upright.
//
// Same safety as MakeThumbnail: DecodeConfig pixel pre-check, orientSem, and
// recover() on decoder panics. On any failure it returns (nil, false) and the
// caller MUST fall back to the original bytes — auto-orient is best-effort
// and never destructive.
func RotateJPEG(data []byte, degCW int) (out []byte, ok bool) {
	if degCW == 0 {
		return data, true
	}
	if degCW != 90 && degCW != 180 && degCW != 270 {
		return nil, false
	}

	defer func() {
		if r := recover(); r != nil {
			slog.Error("image rotate decode panic recovered",
				"panic", r, "data_len", len(data), "deg", degCW)
			out, ok = nil, false
		}
	}()

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	if int64(cfg.Width)*int64(cfg.Height) >= maxThumbnailPixels {
		return nil, false
	}

	orientSem <- struct{}{}
	defer func() { <-orientSem }()

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw == 0 || sh == 0 {
		return nil, false
	}

	// 90/270 swap the output dimensions; 180 keeps them. A multiple-of-90
	// rotation is a lossless pixel permutation, so no interpolation.
	var dst *image.RGBA
	switch degCW {
	case 90:
		dst = image.NewRGBA(image.Rect(0, 0, sh, sw))
		for y := 0; y < sh; y++ {
			for x := 0; x < sw; x++ {
				r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
				// CW 90: source (x,y) -> dest (sh-1-y, x)
				dst.SetRGBA(sh-1-y, x, color.RGBA{
					R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bl >> 8), A: uint8(a >> 8),
				})
			}
		}
	case 180:
		dst = image.NewRGBA(image.Rect(0, 0, sw, sh))
		for y := 0; y < sh; y++ {
			for x := 0; x < sw; x++ {
				r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
				// CW 180: source (x,y) -> dest (sw-1-x, sh-1-y)
				dst.SetRGBA(sw-1-x, sh-1-y, color.RGBA{
					R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bl >> 8), A: uint8(a >> 8),
				})
			}
		}
	case 270:
		dst = image.NewRGBA(image.Rect(0, 0, sh, sw))
		for y := 0; y < sh; y++ {
			for x := 0; x < sw; x++ {
				r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
				// CW 270: source (x,y) -> dest (y, sw-1-x)
				dst.SetRGBA(y, sw-1-x, color.RGBA{
					R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bl >> 8), A: uint8(a >> 8),
				})
			}
		}
	}

	var buf bytes.Buffer
	// Quality 90: this is the user's actual attachment, not a thumbnail.
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90}); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}
