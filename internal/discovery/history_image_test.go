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
	if got, _ := extractTextAndImages(json.RawMessage(raw)); got != "hi" {
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

	entries, ok := parseHistoryLine([]byte(line), 0, true)
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

	entries, ok := parseHistoryLine([]byte(line), 0, true)
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

	if entries, ok := parseHistoryLine([]byte(line), 0, true); ok || len(entries) != 0 {
		t.Errorf("parseHistoryLine ok=%v entries=%d, want ok=false entries=0", ok, len(entries))
	}
}

// TestParseHistoryLine_ToolResultImages: an image a tool returned replays as a
// tool_result entry linked to its call, so a screenshot survives a restart
// when only the Claude JSONL has it.
func TestParseHistoryLine_ToolResultImages(t *testing.T) {
	setThumbnailFn(t, stubThumb)
	toolResult := func(id string, content json.RawMessage) map[string]any {
		return map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
	}
	const lineUUID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	tests := []struct {
		name    string
		content []map[string]any
		want    []string // ToolUseID of each tool_result entry
	}{
		{name: "screenshot", content: []map[string]any{toolResult("tu1", imageBlockContent("shot", b64("png")))}, want: []string{"tu1"}},
		{name: "text-only result dropped", content: []map[string]any{toolResult("tu1", json.RawMessage(`"plain output"`))}},
		{name: "one entry per result", content: []map[string]any{
			toolResult("tu1", imageBlockContent("", b64("a"))),
			toolResult("tu2", imageBlockContent("", b64("b"), b64("c"))),
		}, want: []string{"tu1", "tu2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, _ := json.Marshal(map[string]any{"role": "user", "content": tc.content})
			line := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","uuid":"` + lineUUID + `","message":` + string(msg) + `}`
			entries, ok := parseHistoryLine([]byte(line), 0, true)
			if ok != (len(tc.want) > 0) || len(entries) != len(tc.want) {
				t.Fatalf("ok=%v entries=%d, want %d", ok, len(entries), len(tc.want))
			}
			for i, e := range entries {
				if e.Type != "tool_result" || e.ToolUseID != tc.want[i] || len(e.Images) == 0 || e.Detail != "" {
					t.Errorf("entry %d = {%s %s %d images detail %q}, want tool_result %s with images, no detail",
						i, e.Type, e.ToolUseID, len(e.Images), e.Detail, tc.want[i])
				}
				if e.UUID == "" || e.UUID == strings.ReplaceAll(lineUUID, "-", "") {
					t.Errorf("entry %d UUID = %q, want one derived from the line's", i, e.UUID)
				}
			}
		})
	}
}

// TestParseHistoryLine_ToolResultImagesSkipped: a line newer than the paging
// cursor, or a whole-file preview read, never thumbnails a tool screenshot.
func TestParseHistoryLine_ToolResultImagesSkipped(t *testing.T) {
	thumbs := 0
	setThumbnailFn(t, func(d []byte, n int) string { thumbs++; return stubThumb(d, n) })
	msg, _ := json.Marshal(map[string]any{"role": "user", "content": []map[string]any{
		{"type": "tool_result", "tool_use_id": "tu1", "content": imageBlockContent("", b64("png"))},
	}})
	line := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","uuid":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0","message":` + string(msg) + `}`
	const ts = 1767225600000
	tests := []struct {
		name       string
		beforeMS   int64
		toolImages bool
		want       int
	}{
		{name: "older than cursor", beforeMS: ts + 1, toolImages: true, want: 1},
		{name: "at cursor", beforeMS: ts, toolImages: true},
		{name: "preview read", toolImages: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			thumbs = 0
			entries, _ := parseHistoryLine([]byte(line), tc.beforeMS, tc.toolImages)
			if len(entries) != tc.want || thumbs != tc.want {
				t.Errorf("entries=%d thumbnails=%d, want %d", len(entries), thumbs, tc.want)
			}
		})
	}
}
