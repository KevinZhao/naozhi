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

// Anthropic's vision models downsample any image over ~1568 px on the long
// edge or ~1.15 MP of area before the model sees it, so larger uploads only
// cost base64 bloat — and the stream-json process replays the full transcript
// every turn, so they cost it again on every follow-up. downscaleForVision
// shrinks inline images to that ceiling once, at send time.
const (
	// maxVisionLongEdge matches the backend's longest-edge ceiling.
	maxVisionLongEdge = 1568

	// maxVisionPixels is the total-area ceiling that bites document photos
	// whose long edge is under 1568 (e.g. 1200×1600 = 1.92 MP).
	maxVisionPixels = 1_150_000

	// visionReencodeQuality favours legibility of printed/handwritten text;
	// higher than MakeThumbnail's 70 because this is the model input.
	visionReencodeQuality = 85

	// downscaleSkipRatio: scales this close to 1.0 return the original bytes
	// rather than pay a lossy re-encode for a sub-percent change.
	downscaleSkipRatio = 0.995
)

// downscaleImagesForVision returns a new slice in which every inline raster
// image over the vision resize thresholds has been shrunk. Never mutates the
// input; best-effort — undecodable images, file_ref attachments and re-encode
// failures pass through byte-for-byte. Applied at the CLI write boundary so
// every transcript replay reuses the smaller bytes.
func downscaleImagesForVision(images []Attachment) []Attachment {
	if len(images) == 0 {
		return images
	}
	out := make([]Attachment, len(images))
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

// downscaleForVision shrinks one image to the vision edge + area limits and
// re-encodes to JPEG. Returns (bytes, mime, true) on resize, or
// (nil, "", false) when already within limits, undecodable or un-encodable —
// the caller keeps the original bytes. Same safety as MakeThumbnail:
// DecodeConfig pixel pre-check, thumbSem, and recover() on decoder panics.
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
	// Same ceiling as thumbnail / rotate: a 4096×4096 RGBA decode is 64 MB.
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

	thumbSem <- struct{}{}
	defer func() { <-thumbSem }()

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", false
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// JPEG has no alpha: pre-fill white (the RGBA zero value is transparent
	// BLACK) so transparent PNG regions flatten onto the natural document
	// backdrop, then composite the scaled source Over it.
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	// CatmullRom keeps text legible on downscale (OCR-heavy workload).
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
