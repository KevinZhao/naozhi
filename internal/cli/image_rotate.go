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

// orientSem caps concurrent auto-orient decodes independently of thumbSem.
//
// R20260606-GO-2 (#1844): auto-orient is fire-and-forget and best-effort, but
// it ran through the same thumbSem (cap=4) that serialises MakeThumbnail. A
// burst of uploaded images could fill all 4 image-decode slots with orient
// work and starve the user-visible thumbnail generation, which is the higher
// priority. A dedicated, smaller semaphore keeps orient bounded for memory
// while leaving thumbSem exclusively for thumbnails.
var orientSem = make(chan struct{}, 2)

// RotateJPEG decodes raw image bytes, rotates the pixels clockwise by
// `degCW` (must be 90, 180, or 270), and re-encodes to JPEG. degCW==0
// returns the input unchanged. Any other value is rejected.
//
// Rationale: phone/scanner images that carry NO EXIF orientation flag
// (e.g. a sideways document photo) can't be corrected by the
// createImageBitmap(from-image) path — the bytes are physically rotated
// with nothing to signal it. The auto-orient feature asks a vision model
// which way is up and bakes the rotation into the pixels here so every
// downstream consumer (Claude, the dashboard lightbox, IM channels) sees
// an upright image.
//
// Safety mirrors MakeThumbnail (thumbnail.go): a DecodeConfig pre-check
// caps pixel count before the full RGBA decode to bound memory, a dedicated
// orientSem cap serialises concurrent orient decodes (kept separate from
// thumbSem so best-effort orient can't starve user-visible thumbnails), and
// a recover() treats a decoder panic on crafted-malformed input as a failure
// rather than crashing the process.
//
// On any failure (bad degrees, undecodable input, oversize, encode error,
// decoder panic) it returns (nil, false) and the caller MUST fall back to
// the original bytes — auto-orient is best-effort and never destructive.
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

	// For 90/270 the output dimensions swap; for 180 they stay. The
	// destination pixel (dx,dy) is filled from the source coordinate that
	// maps onto it under a clockwise rotation. Nearest-neighbour copy with
	// no interpolation — a multiple-of-90 rotation is a lossless pixel
	// permutation, so there's nothing to interpolate.
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
	// Quality 90: this is the user's actual attachment (not a thumbnail),
	// so preserve more detail than MakeThumbnail's 70. The frontend already
	// downscaled to <=1600px / q0.8 in normalizeImage, so a re-encode here
	// is a one-time, bounded quality cost.
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90}); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}
