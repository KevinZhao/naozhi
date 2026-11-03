package cli

import (
	"bytes"
	"encoding/base64"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/datadir"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, newSolidImage(8, 8, color.RGBA{0, 0, 255, 255})); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEventEntriesFromEventAt_ToolResultImages(t *testing.T) {
	t.Parallel()
	pngData := testPNG(t)
	img := func(path string) clievent.ToolImage {
		return clievent.ToolImage{MimeType: "image/png", Data: pngData, Path: path}
	}
	bad := clievent.ToolImage{MimeType: "image/png", Data: []byte("not an image"), Path: "planner/attachments/d/bad.png"}

	type want struct {
		toolUseID string
		summary   string
		images    int
		paths     []string
	}
	tests := []struct {
		name    string
		results []clievent.ToolResultImages
		want    []want
	}{
		{name: "tool result without images", want: nil},
		{
			name:    "screenshot with stored original",
			results: []clievent.ToolResultImages{{ToolUseID: "tu1", Text: "Took the screenshot", Images: []clievent.ToolImage{img("planner/attachments/d/a.png")}}},
			want:    []want{{toolUseID: "tu1", summary: "Took the screenshot", images: 1, paths: []string{"planner/attachments/d/a.png"}}},
		},
		{
			name:    "original not stored keeps the thumbnail only",
			results: []clievent.ToolResultImages{{ToolUseID: "tu1", Images: []clievent.ToolImage{img("")}}},
			want:    []want{{toolUseID: "tu1", images: 1}},
		},
		{
			name:    "undecodable image dropped with its path",
			results: []clievent.ToolResultImages{{ToolUseID: "tu1", Images: []clievent.ToolImage{bad, img("planner/attachments/d/b.png")}}},
			want:    []want{{toolUseID: "tu1", images: 1, paths: []string{"planner/attachments/d/b.png"}}},
		},
		{
			name:    "only undecodable images yield no entry",
			results: []clievent.ToolResultImages{{ToolUseID: "tu1", Images: []clievent.ToolImage{bad}}},
			want:    nil,
		},
		{
			name: "one entry per tool result",
			results: []clievent.ToolResultImages{
				{ToolUseID: "tu1", Images: []clievent.ToolImage{img("")}},
				{ToolUseID: "tu2", Images: []clievent.ToolImage{img(""), img("")}},
			},
			want: []want{{toolUseID: "tu1", images: 1}, {toolUseID: "tu2", images: 2}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := clievent.Event{Type: "user", ToolResultImages: tc.results}
			got := EventEntriesFromEventAt(ev, 1000)
			if len(got) != len(tc.want) {
				t.Fatalf("entries = %d, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				e := got[i]
				if e.Type != clievent.KindToolResult || e.ToolUseID != w.toolUseID || e.Summary != w.summary || e.Time != 1000 {
					t.Errorf("entry %d = {%s %s %q %d}, want {tool_result %s %q 1000}", i, e.Type, e.ToolUseID, e.Summary, e.Time, w.toolUseID, w.summary)
				}
				if e.Detail != "" {
					t.Errorf("entry %d Detail = %q, want empty (image-only contentKey)", i, e.Detail)
				}
				if len(e.Images) != w.images {
					t.Fatalf("entry %d images = %d, want %d", i, len(e.Images), w.images)
				}
				for _, th := range e.Images {
					if !strings.HasPrefix(th, "data:image/jpeg;base64,") {
						t.Errorf("thumbnail %.40q is not a JPEG data URI", th)
					}
				}
				if strings.Join(e.ImagePaths, ",") != strings.Join(w.paths, ",") {
					t.Errorf("entry %d ImagePaths = %v, want %v", i, e.ImagePaths, w.paths)
				}
			}
		})
	}
}

func TestReadEvent_ToolResultImages(t *testing.T) {
	t.Parallel()
	pngData := testPNG(t)
	b64 := base64.StdEncoding.EncodeToString(pngData)
	image := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}`
	tests := []struct {
		name string
		line string
		want []clievent.ToolResultImages
	}{
		{
			name: "text and image",
			line: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"a"},` + image + `,{"type":"text","text":"b"}]}]}}`,
			want: []clievent.ToolResultImages{{ToolUseID: "tu1", Text: "a\nb", Images: []clievent.ToolImage{{MimeType: "image/png", Data: pngData}}}},
		},
		{
			name: "string content and image-less results skipped",
			line: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"\"type\":\"image\""},{"type":"tool_result","tool_use_id":"tu2","content":[{"type":"text","text":"x"}]},{"type":"tool_result","tool_use_id":"tu3","content":[` + image + `]}]}}`,
			want: []clievent.ToolResultImages{{ToolUseID: "tu3", Images: []clievent.ToolImage{{MimeType: "image/png", Data: pngData}}}},
		},
		{
			name: "bad base64 dropped",
			line: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"!!"}}]}]}}`,
		},
		{
			name: "replay echo ignored",
			line: `{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[` + image + `]}]}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			evs, _, err := (&ClaudeProtocol{}).ReadEvent(tc.line)
			if err != nil || len(evs) != 1 {
				t.Fatalf("ReadEvent = %d events, err %v", len(evs), err)
			}
			got := evs[0].ToolResultImages
			if len(got) != len(tc.want) {
				t.Fatalf("results = %d, want %d", len(got), len(tc.want))
			}
			for i, w := range tc.want {
				g := got[i]
				if g.ToolUseID != w.ToolUseID || g.Text != w.Text || len(g.Images) != len(w.Images) {
					t.Fatalf("result %d = {%s %q %d images}, want {%s %q %d images}", i, g.ToolUseID, g.Text, len(g.Images), w.ToolUseID, w.Text, len(w.Images))
				}
				for j := range w.Images {
					if g.Images[j].MimeType != w.Images[j].MimeType || !bytes.Equal(g.Images[j].Data, w.Images[j].Data) {
						t.Errorf("result %d image %d differs", i, j)
					}
				}
			}
		})
	}
}

func TestStoreToolImages(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	pngData := testPNG(t)
	results := []clievent.ToolResultImages{{Images: []clievent.ToolImage{
		{MimeType: "image/png", Data: pngData},
		{MimeType: "image/svg+xml", Data: []byte("<svg/>")},
	}}}
	(&Process{stateDir: stateDir}).storeToolImages(results)

	stored, skipped := results[0].Images[0].Path, results[0].Images[1].Path
	if skipped != "" {
		t.Errorf("svg stored at %q, want it skipped", skipped)
	}
	if !strings.HasPrefix(stored, datadir.PlannerAttachmentsRel+"/") || !strings.HasSuffix(stored, ".png") {
		t.Fatalf("Path = %q, want %s/<date>/<id>.png", stored, datadir.PlannerAttachmentsRel)
	}
	abs := filepath.Join(stateDir, filepath.FromSlash(stored))
	if got, err := os.ReadFile(abs); err != nil || !bytes.Equal(got, pngData) {
		t.Fatalf("original at %s: err %v, equal %v", abs, err, bytes.Equal(got, pngData))
	}
	if _, err := os.Stat(strings.TrimSuffix(abs, ".png") + ".meta"); err != nil {
		t.Errorf("meta sidecar: %v", err)
	}

	// No state dir: nothing is written and no path is recorded.
	none := []clievent.ToolResultImages{{Images: []clievent.ToolImage{{MimeType: "image/png", Data: pngData}}}}
	(&Process{}).storeToolImages(none)
	if none[0].Images[0].Path != "" {
		t.Errorf("Path = %q with no state dir, want empty", none[0].Images[0].Path)
	}
}
