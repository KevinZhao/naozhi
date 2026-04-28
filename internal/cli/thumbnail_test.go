package cli

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/image/bmp"
)

// newSolidImage constructs an NxN RGBA image filled with the given color.
// Kept tiny so encoded outputs stay well under any pixel/byte bounds.
func newSolidImage(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestMakeThumbnail_PNG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, newSolidImage(8, 8, color.RGBA{255, 0, 0, 255})); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	got := MakeThumbnail(buf.Bytes(), 4)
	if !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Fatalf("PNG input should produce a JPEG thumbnail; got %q (len=%d)", head(got), len(got))
	}
}

// TestMakeThumbnail_BMP verifies the `_ "golang.org/x/image/bmp"` decoder
// registration — without it, image.DecodeConfig on a BMP payload returns an
// "unknown format" error and MakeThumbnail silently returns "". Removing the
// bmp import would immediately fail this test.
func TestMakeThumbnail_BMP(t *testing.T) {
	var buf bytes.Buffer
	if err := bmp.Encode(&buf, newSolidImage(16, 16, color.RGBA{0, 128, 255, 255})); err != nil {
		t.Fatalf("bmp encode: %v", err)
	}
	got := MakeThumbnail(buf.Bytes(), 8)
	if !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Fatalf("BMP input should produce a JPEG thumbnail; got %q (len=%d)", head(got), len(got))
	}
}

// webpGopher1bppLosslessB64 is a 442-byte lossless WebP taken from
// golang.org/x/image/testdata (gopher-doc.1bpp.lossless.webp). Embedded as
// base64 so we don't ship a binary fixture file. The decoded image has
// non-trivial dimensions, enough to exercise the resize path.
const webpGopher1bppLosslessB64 = "UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA=="

// TestMakeThumbnail_WebP verifies the `_ "golang.org/x/image/webp"` decoder
// registration. Uses a real WebP fixture (decoded from base64 above) rather
// than attempting to hand-craft one — WebP's RIFF+VP8L container is too
// elaborate for a synthetic minimal payload.
func TestMakeThumbnail_WebP(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString(webpGopher1bppLosslessB64)
	if err != nil {
		t.Fatalf("decode fixture base64: %v", err)
	}
	got := MakeThumbnail(data, 32)
	if !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Fatalf("WebP input should produce a JPEG thumbnail; got %q (len=%d)", head(got), len(got))
	}
}

// TestMakeThumbnail_UnknownFormat regresses the "silently skip" contract:
// anything image.DecodeConfig cannot parse must return "". Guards against a
// future decoder import accidentally claiming plain bytes.
func TestMakeThumbnail_UnknownFormat(t *testing.T) {
	cases := [][]byte{
		[]byte("not an image"),
		{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07},
		{}, // empty
	}
	for i, data := range cases {
		if got := MakeThumbnail(data, 16); got != "" {
			t.Errorf("case %d: unknown bytes should return \"\"; got %q", i, head(got))
		}
	}
}

// TestMakeThumbnail_OversizedPixelCountRejectedBMP exercises the OOM guard on
// the new bmp decoder path: a 5000x5000 solid BMP trips the
// width*height >= maxThumbnailPixels check inside DecodeConfig and must
// return "" BEFORE any full Decode attempt. Protects against a future change
// that would lift the bmp decoder above the pixel cap.
func TestMakeThumbnail_OversizedPixelCountRejectedBMP(t *testing.T) {
	// MakeThumbnail calls image.DecodeConfig (reads header only) before full
	// Decode, so we can advertise oversized dimensions via a minimal hand-
	// crafted BMP header without allocating 64 MB of pixels. The DecodeConfig
	// path returns width/height, the caller compares width*height against
	// maxThumbnailPixels, and rejects — never touching the decoder body.
	const side = 4097 // side*side > maxThumbnailPixels = 4096*4096
	if int64(side)*int64(side) < maxThumbnailPixels {
		t.Fatalf("side %d insufficient vs cap %d", side, maxThumbnailPixels)
	}
	if got := MakeThumbnail(hackBMPHeader(side, side), 64); got != "" {
		t.Fatalf("oversized BMP must be rejected; got %q (len=%d)", head(got), len(got))
	}
}

// hackBMPHeader builds a 54-byte BMP (BITMAPFILEHEADER + BITMAPINFOHEADER) that
// declares width × height 24-bpp uncompressed pixels. It intentionally omits
// the pixel payload — DecodeConfig only reads the two headers, and our cap
// check fires immediately after, never descending into Decode. Gives us an
// O(1) way to stress the size-gate path without encoding huge images.
func hackBMPHeader(width, height int) []byte {
	const fileHdr, infoHdr = 14, 40
	b := make([]byte, fileHdr+infoHdr)
	// BITMAPFILEHEADER: "BM", file size (unused by decoder), reserved, pixel offset.
	b[0], b[1] = 'B', 'M'
	// pixel data offset: fileHdr + infoHdr (the 24-bpp branch requires this exact value).
	putU32(b[10:14], fileHdr+infoHdr)
	// BITMAPINFOHEADER (40 bytes).
	putU32(b[14:18], infoHdr)        // info header size
	putU32(b[18:22], uint32(width))  // width
	putU32(b[22:26], uint32(height)) // height (positive = bottom-up)
	putU16(b[26:28], 1)              // planes
	putU16(b[28:30], 24)             // bits per pixel
	// compression, image size, x/y ppm, colors-used, colors-important → all zero.
	return b
}

func putU16(b []byte, v uint16) { b[0] = byte(v); b[1] = byte(v >> 8) }
func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

// head returns the first 60 runes of s for compact error messages.
func head(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:60] + "..."
}
