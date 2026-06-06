package claudejsonl

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

// makePNGBase64 renders a w×h solid-colour PNG and returns its base64
// encoding, standing in for a real inline image in a Claude JSONL line.
func makePNGBase64(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 120, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func userImageLineAt(t *testing.T, text, imgB64 string, unixSec int64) string {
	t.Helper()
	blocks := []map[string]any{}
	if text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	blocks = append(blocks, map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": "image/png",
			"data":       imgB64,
		},
	})
	content, _ := json.Marshal(blocks)
	msg, _ := json.Marshal(map[string]any{"role": "user", "content": json.RawMessage(content)})
	ts := time.Unix(unixSec, 0).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"uuid":"img-%d","message":%s}`, ts, unixSec, string(msg))
}

// TestSource_LoadBefore_DecodesHistoryImages is the end-to-end check that the
// package init wired discovery.ThumbnailFn to cli.MakeThumbnail: a Claude
// JSONL line carrying an inline image is rehydrated with a downsampled
// thumbnail data URI, not dropped to text-only.
func TestSource_LoadBefore_DecodesHistoryImages(t *testing.T) {
	if discovery.ThumbnailFn == nil {
		t.Fatal("discovery.ThumbnailFn is nil — claudejsonl.init must wire cli.MakeThumbnail")
	}

	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/cjsonl-img"
	dirName := projDirName(cwd)
	sessID := "33333333-3333-3333-3333-333333333cc3"

	// A source image larger than the 600px thumbnail cap so we can assert
	// the result was downsampled (smaller than the original bytes).
	srcB64 := makePNGBase64(t, 1200, 900)
	line := userImageLineAt(t, "看这张图", srcB64, 1000)
	writeSessionJSONL(t, claudeDir, dirName, sessID, []string{line})

	src := New(claudeDir, cwd, func() []string { return []string{sessID} })
	entries, err := src.LoadBefore(context.Background(), int64(2000)*1000, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(entries))
	}
	e := entries[0]
	if e.Summary != "看这张图" {
		t.Errorf("Summary = %q, want %q", e.Summary, "看这张图")
	}
	if len(e.Images) != 1 {
		t.Fatalf("Images = %v, want exactly one thumbnail", e.Images)
	}
	thumb := e.Images[0]
	if !strings.HasPrefix(thumb, "data:image/jpeg;base64,") {
		t.Errorf("thumbnail = %q, want data:image/jpeg;base64, prefix", thumb[:min(40, len(thumb))])
	}
	// Downsampling must have shrunk the payload well below the original.
	if len(thumb) >= len(srcB64) {
		t.Errorf("thumbnail len %d not smaller than source len %d — downsample failed", len(thumb), len(srcB64))
	}
}

// TestSource_LoadBefore_OversizedImageNoOOM feeds an image whose pixel count
// exceeds MakeThumbnail's pre-check cap. The thumbnail must be dropped (empty)
// without crashing, and the text must survive.
func TestSource_LoadBefore_CorruptImageKeepsText(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/cjsonl-img-bad"
	dirName := projDirName(cwd)
	sessID := "44444444-4444-4444-4444-444444444dd4"

	// Not valid base64-encoded image bytes: decode-as-image fails, image
	// dropped, text kept.
	line := userImageLineAt(t, "仅文字应保留", base64.StdEncoding.EncodeToString([]byte("not-a-real-image")), 1000)
	writeSessionJSONL(t, claudeDir, dirName, sessID, []string{line})

	src := New(claudeDir, cwd, func() []string { return []string{sessID} })
	entries, err := src.LoadBefore(context.Background(), int64(2000)*1000, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(entries))
	}
	if entries[0].Summary != "仅文字应保留" {
		t.Errorf("Summary = %q, want %q", entries[0].Summary, "仅文字应保留")
	}
	if len(entries[0].Images) != 0 {
		t.Errorf("Images = %v, want none (undecodable image dropped)", entries[0].Images)
	}
}
