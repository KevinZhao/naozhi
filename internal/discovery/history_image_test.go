package discovery

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// setThumbnailFn installs a ThumbnailFn for the duration of one test and
// restores the previous value on cleanup. ThumbnailFn is a process-global
// injection point, so tests that touch it must NOT run in parallel with one
// another — none of the tests below call t.Parallel().
func setThumbnailFn(t *testing.T, fn func(data []byte, maxDim int) string) {
	t.Helper()
	prev := ThumbnailFn
	ThumbnailFn = fn
	t.Cleanup(func() { ThumbnailFn = prev })
}

// stubThumb returns a fixed, well-formed data URI regardless of input so the
// discovery layer can be tested without pulling in a real image decoder.
func stubThumb(_ []byte, _ int) string {
	return dataURIPrefix + "jpeg;base64,STUBTHUMBNAIL"
}

// b64 wraps arbitrary bytes as a base64 string for inline JSONL content.
func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// imageBlockContent builds a content []block JSON value mixing text and image
// blocks. Each image is given the supplied base64 data with a base64 source.
func imageBlockContent(text string, imageData ...string) json.RawMessage {
	var blocks []map[string]any
	if text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	for _, d := range imageData {
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "image/jpeg",
				"data":       d,
			},
		})
	}
	out, _ := json.Marshal(blocks)
	return out
}

func TestExtractTextAndImages(t *testing.T) {
	setThumbnailFn(t, stubThumb)

	tests := []struct {
		name       string
		raw        string
		wantText   string
		wantImages int
	}{
		{
			name:       "plain string no images",
			raw:        `"hello"`,
			wantText:   "hello",
			wantImages: 0,
		},
		{
			name:       "text blocks only",
			raw:        `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`,
			wantText:   "a\nb",
			wantImages: 0,
		},
		{
			name:       "text plus one image",
			raw:        `[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + b64("rawbytes") + `"}}]`,
			wantText:   "hi",
			wantImages: 1,
		},
		{
			name:       "image only no text",
			raw:        `[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + b64("rawbytes") + `"}}]`,
			wantText:   "",
			wantImages: 1,
		},
		{
			name:       "two images",
			raw:        `[{"type":"image","source":{"type":"base64","data":"` + b64("a") + `"}},{"type":"image","source":{"type":"base64","data":"` + b64("b") + `"}}]`,
			wantText:   "",
			wantImages: 2,
		},
		{
			name:       "corrupt base64 skipped text kept",
			raw:        `[{"type":"text","text":"keep"},{"type":"image","source":{"type":"base64","data":"!!!notbase64!!!"}}]`,
			wantText:   "keep",
			wantImages: 0,
		},
		{
			name:       "non base64 source type skipped",
			raw:        `[{"type":"image","source":{"type":"url","data":"` + b64("x") + `"}}]`,
			wantText:   "",
			wantImages: 0,
		},
		{
			name:       "image block missing source skipped",
			raw:        `[{"type":"image"},{"type":"text","text":"t"}]`,
			wantText:   "t",
			wantImages: 0,
		},
		{
			name:       "tool_use block ignored",
			raw:        `[{"type":"tool_use","name":"bash"},{"type":"text","text":"v"}]`,
			wantText:   "v",
			wantImages: 0,
		},
		{
			name:       "empty",
			raw:        `""`,
			wantText:   "",
			wantImages: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, images := extractTextAndImages(json.RawMessage(tc.raw))
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
			if len(images) != tc.wantImages {
				t.Fatalf("len(images) = %d, want %d (%v)", len(images), tc.wantImages, images)
			}
			for i, img := range images {
				if !strings.HasPrefix(img, dataURIPrefix) {
					t.Errorf("images[%d] = %q, want data URI prefix %q", i, img, dataURIPrefix)
				}
			}
		})
	}
}

// TestExtractTextAndImages_NilThumbnailFn verifies the un-wired path: image
// blocks are silently skipped (text preserved, no panic), matching pre-hook
// behaviour.
func TestExtractTextAndImages_NilThumbnailFn(t *testing.T) {
	setThumbnailFn(t, nil)
	raw := `[{"type":"text","text":"only text"},{"type":"image","source":{"type":"base64","data":"` + b64("x") + `"}}]`
	text, images := extractTextAndImages(json.RawMessage(raw))
	if text != "only text" {
		t.Errorf("text = %q, want %q", text, "only text")
	}
	if len(images) != 0 {
		t.Errorf("len(images) = %d, want 0 when ThumbnailFn is nil", len(images))
	}
}

// TestExtractTextAndImages_ThumbnailReturnsEmpty simulates a decode failure or
// oversized image: ThumbnailFn returns "" and the image is filtered out
// without dropping text or panicking.
func TestExtractTextAndImages_ThumbnailReturnsEmpty(t *testing.T) {
	setThumbnailFn(t, func(_ []byte, _ int) string { return "" })
	raw := `[{"type":"text","text":"t"},{"type":"image","source":{"type":"base64","data":"` + b64("big") + `"}}]`
	text, images := extractTextAndImages(json.RawMessage(raw))
	if text != "t" {
		t.Errorf("text = %q, want %q", text, "t")
	}
	if len(images) != 0 {
		t.Errorf("len(images) = %d, want 0 when thumbnail empty", len(images))
	}
}

// TestExtractText_BackwardCompat confirms the thin wrapper still returns only
// text and ignores images.
func TestExtractText_BackwardCompat(t *testing.T) {
	setThumbnailFn(t, stubThumb)
	raw := `[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","data":"` + b64("x") + `"}}]`
	if got := extractText(json.RawMessage(raw)); got != "hi" {
		t.Errorf("extractText = %q, want %q", got, "hi")
	}
}

// TestParseHistoryLine_UserWithImage covers the parseHistoryLine user branch:
// an image block must populate EventEntry.Images with a data URI while leaving
// ImagePaths empty and the summary untouched (so UUID derivation is stable).
func TestParseHistoryLine_UserWithImage(t *testing.T) {
	setThumbnailFn(t, stubThumb)
	content := imageBlockContent("看图", b64("rawimage"))
	msg, _ := json.Marshal(map[string]any{"role": "user", "content": content})
	line := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","uuid":"u1","message":` + string(msg) + `}`

	entries, ok := parseHistoryLine([]byte(line))
	if !ok || len(entries) != 1 {
		t.Fatalf("parseHistoryLine ok=%v entries=%d, want ok=true entries=1", ok, len(entries))
	}
	e := entries[0]
	if e.Type != "user" {
		t.Errorf("Type = %q, want user", e.Type)
	}
	if e.Summary != "看图" {
		t.Errorf("Summary = %q, want %q (must not be mutated)", e.Summary, "看图")
	}
	if len(e.Images) != 1 || !strings.HasPrefix(e.Images[0], dataURIPrefix) {
		t.Fatalf("Images = %v, want one data URI", e.Images)
	}
	if len(e.ImagePaths) != 0 {
		t.Errorf("ImagePaths = %v, want empty (JSONL has no workspace path)", e.ImagePaths)
	}
}

// TestParseHistoryLine_ImageOnlyNotDropped verifies an image-only user turn
// (no text) is still surfaced rather than discarded by the text=="" guard.
func TestParseHistoryLine_ImageOnlyNotDropped(t *testing.T) {
	setThumbnailFn(t, stubThumb)
	content := imageBlockContent("", b64("rawimage"))
	msg, _ := json.Marshal(map[string]any{"role": "user", "content": content})
	line := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","uuid":"u2","message":` + string(msg) + `}`

	entries, ok := parseHistoryLine([]byte(line))
	if !ok || len(entries) != 1 {
		t.Fatalf("parseHistoryLine ok=%v entries=%d, want ok=true entries=1", ok, len(entries))
	}
	if len(entries[0].Images) != 1 {
		t.Errorf("Images = %v, want one image for image-only turn", entries[0].Images)
	}
}

// TestParseHistoryLine_NoTextNoImageDropped confirms a content with neither
// usable text nor images is still dropped.
func TestParseHistoryLine_NoTextNoImageDropped(t *testing.T) {
	setThumbnailFn(t, stubThumb)
	content := `[{"type":"tool_use","name":"bash"}]`
	msg, _ := json.Marshal(map[string]any{"role": "user", "content": json.RawMessage(content)})
	line := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","uuid":"u3","message":` + string(msg) + `}`

	if entries, ok := parseHistoryLine([]byte(line)); ok || len(entries) != 0 {
		t.Errorf("parseHistoryLine ok=%v entries=%d, want ok=false entries=0", ok, len(entries))
	}
}
