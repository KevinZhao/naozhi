package cli

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
)

// MakeThumbnail generates a small JPEG data URI from raw image bytes.
// Returns empty string if the image cannot be decoded.
func MakeThumbnail(data []byte, maxDim int) string {
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
		if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 60}); err != nil {
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
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 60}); err != nil {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}
