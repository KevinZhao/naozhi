package cli

import (
	"bytes"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log/slog"
	"math"

	"golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// Anthropic's vision models resize any image that exceeds ~1568 px on its
// longest edge OR ~1.15 megapixels of area before the model ever sees it
// (a larger image is downsampled server-side to fit within a fixed tile
// budget, ~1600 vision tokens max). Sending a larger image therefore buys
// nothing: you pay the upload + base64 bloat + a redundant server-side
// resize, and — because the stream-json process replays the full transcript
// every turn — you pay it AGAIN on every follow-up in the same session.
//
// downscaleForVision shrinks an inline image to the largest size that is
// still fully honoured by the model, so every turn ships the minimum bytes
// with zero loss of information the model would actually have used. A 1200×1600
// photo (1.92 MP) becomes ~928×1238 (1.15 MP): the exact resolution the API
// would have downsampled it to, but computed once at send time.
const (
	// maxVisionLongEdge is the longest-edge ceiling above which the vision
	// backend downsamples. Match it so we never send pixels the model drops.
	maxVisionLongEdge = 1568

	// maxVisionPixels is the total-area ceiling (~1.15 MP). This is the knee
	// that bites tall/wide document photos whose long edge is under 1568 but
	// whose area still exceeds the tile budget (e.g. 1200×1600 = 1.92 MP).
	maxVisionPixels = 1_150_000

	// visionReencodeQuality balances legibility of printed/handwritten text
	// (the dominant vision workload here) against payload size. Higher than
	// MakeThumbnail's 70 because this is the actual model input, not a preview.
	visionReencodeQuality = 85

	// downscaleSkipRatio: if the computed scale is within this fraction of 1.0
	// the image is already effectively at target size, so we return the
	// original bytes untouched rather than paying a lossy re-encode for a
	// sub-percent dimension change.
	downscaleSkipRatio = 0.995
)

// downscaleImagesForVision returns a new slice in which every inline raster
// image larger than the vision backend's resize thresholds has been shrunk to
// fit. It is immutable (never mutates the input slice or its ImageData values)
// and best-effort: any image that cannot be decoded, is a non-image
// attachment (file_ref), or fails to re-encode is passed through byte-for-byte.
// Callers apply this at the CLI write boundary so the shrink happens once and
// every subsequent transcript replay reuses the smaller bytes.
func downscaleImagesForVision(images []ImageData) []ImageData {
	if len(images) == 0 {
		return images
	}
	out := make([]ImageData, len(images))
	copy(out, images)
	for i := range out {
		img := out[i]
		// Only inline raster bytes are eligible. file_ref carries no Data.
		if img.Kind == KindFileRef || len(img.Data) == 0 {
			continue
		}
		data, mime, changed := downscaleForVision(img.Data)
		if !changed {
			continue
		}
		out[i].Data = data
		out[i].MimeType = mime
	}
	return out
}

// downscaleForVision shrinks a single image's raw bytes to fit within the
// vision backend's edge + area limits, re-encoding to JPEG. It returns
// (newBytes, newMimeType, true) when a resize happened, or (nil, "", false)
// when the image is already within limits, cannot be decoded, or fails to
// re-encode — in every false case the caller keeps the original bytes.
//
// Safety mirrors RotateJPEG / MakeThumbnail: a DecodeConfig pre-check caps the
// pixel count before the full RGBA decode (bounding memory), the shared
// thumbSem serialises concurrent decodes, and a recover() treats a decoder
// panic on crafted-malformed input as a pass-through rather than a crash.
func downscaleForVision(data []byte) (out []byte, mime string, changed bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("image downscale decode panic recovered",
				"panic", r, "data_len", len(data))
			out, mime, changed = nil, "", false
		}
	}()

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", false
	}
	sw, sh := cfg.Width, cfg.Height
	if sw <= 0 || sh <= 0 {
		return nil, "", false
	}
	// Refuse to decode absurdly large sources (same ceiling as thumbnail /
	// rotate) — a full RGBA decode of a 4096×4096 image is already 64 MB.
	if int64(sw)*int64(sh) >= maxThumbnailPixels {
		return nil, "", false
	}

	scale := targetScale(sw, sh)
	if scale >= downscaleSkipRatio {
		return nil, "", false // already within limits; leave bytes untouched.
	}
	dw := int(math.Round(float64(sw) * scale))
	dh := int(math.Round(float64(sh) * scale))
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}

	// Limit concurrent decodes to cap aggregate memory usage. Reuse thumbSem —
	// downscale, like MakeThumbnail, runs on the outbound send path.
	thumbSem <- struct{}{}
	defer func() { <-thumbSem }()

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", false
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// JPEG has no alpha channel, so transparent regions of a source PNG must
	// be flattened onto a solid background. Pre-fill white (not the RGBA zero
	// value, which is transparent BLACK) so a cropped/rounded screenshot or a
	// diagram with a transparent background comes out on white — the natural
	// backdrop for the document/OCR vision workload — instead of darkened.
	// Then composite the scaled source Over the white fill.
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	// CatmullRom keeps printed/handwritten text legible on downscale far
	// better than nearest-neighbour — the vision workload here is OCR-heavy.
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: visionReencodeQuality}); err != nil {
		return nil, "", false
	}
	return buf.Bytes(), "image/jpeg", true
}

// targetScale returns the largest scale factor <= 1.0 that brings a sw×sh
// image within BOTH the long-edge and total-area vision limits. A return of
// 1.0 means the image is already within limits.
func targetScale(sw, sh int) float64 {
	scale := 1.0
	long := sw
	if sh > long {
		long = sh
	}
	if long > maxVisionLongEdge {
		scale = float64(maxVisionLongEdge) / float64(long)
	}
	if area := float64(sw) * float64(sh); area > maxVisionPixels {
		if s := math.Sqrt(maxVisionPixels / area); s < scale {
			scale = s
		}
	}
	return scale
}
