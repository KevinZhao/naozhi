package cli

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log/slog"

	// Decoder-only registration for webp/bmp uploads accepted by the
	// dashboard and Discord adapter; output is always normalized to JPEG.
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// maxThumbnailPixels is the maximum source image pixel count allowed for
// thumbnail generation. Larger images are skipped to prevent OOM from the
// full RGBA decode (e.g., 4096x4096 = 64 MB RGBA).
const maxThumbnailPixels = 4096 * 4096

// thumbnailWorkerCap bounds concurrent image decodes and is also the
// worker-pool size process_send.go uses per multi-image message, so the two
// cannot drift (#569).
const thumbnailWorkerCap = 4

// thumbSem limits concurrent image decode operations to cap aggregate memory.
var thumbSem = make(chan struct{}, thumbnailWorkerCap)

// MakeThumbnail generates a small JPEG data URI from raw image bytes.
// Returns empty string if the image cannot be decoded or is too large.
//
// The pure-Go decoders can panic on crafted inputs and the bytes are
// user-supplied (dashboard / IM adapters), so a decoder panic is treated as a
// decode failure (logged at Error so it stays observable) instead of taking
// down the process.
func MakeThumbnail(data []byte, maxDim int) (result string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("thumbnail decode panic recovered",
				"panic", r, "data_len", len(data))
			result = ""
		}
	}()
	// Pre-check dimensions without full decode to prevent OOM on large images.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	if int64(cfg.Width)*int64(cfg.Height) >= maxThumbnailPixels {
		return ""
	}

	// Limit concurrent decodes to cap aggregate memory usage.
	thumbSem <- struct{}{}
	defer func() { <-thumbSem }()

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw == 0 || sh == 0 {
		return ""
	}

	dw, dh := sw, sh
	if sw >= sh && sw > maxDim {
		dw = maxDim
		dh = sh * maxDim / sw
	} else if sh > maxDim {
		dh = maxDim
		dw = sw * maxDim / sh
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}

	// No resize needed
	if dw == sw && dh == sh {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 70}); err != nil {
			return ""
		}
		return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	}

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		sy := b.Min.Y + y*sh/dh
		for x := range dw {
			sx := b.Min.X + x*sw/dw
			r, g, bl, a := src.At(sx, sy).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bl >> 8), A: uint8(a >> 8),
			})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 70}); err != nil {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}
