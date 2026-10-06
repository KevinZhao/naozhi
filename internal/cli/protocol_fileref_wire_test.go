package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// fileRefWireCase is one attachment mix sent through a JSON-RPC backend.
type fileRefWireCase struct {
	name       string
	text       string
	atts       []clievent.Attachment
	wantImages int
}

var fileRefPDF = clievent.Attachment{
	Kind:          clievent.KindFileRef,
	MimeType:      "application/pdf",
	WorkspacePath: ".naozhi/attachments/2026-10-06/abc.pdf",
	OrigName:      "report.pdf",
	Size:          2048,
}

var fileRefWireCases = []fileRefWireCase{
	{name: "file_ref_only", text: "", atts: []clievent.Attachment{fileRefPDF}},
	{name: "file_ref_with_text", text: "summarize", atts: []clievent.Attachment{fileRefPDF}},
	{
		name: "image_and_file_ref",
		text: "compare",
		atts: []clievent.Attachment{
			{Kind: clievent.KindImageInline, MimeType: "image/png", Data: []byte{0x89, 0x50}},
			fileRefPDF,
		},
		wantImages: 1,
	},
}

// checkFileRefWire asserts a backend's user-turn blocks: images only for
// inline attachments, and one text block carrying the Read hint plus the
// user's text.
func checkFileRefWire(t *testing.T, tc fileRefWireCase, types []string, media []string, texts []string) {
	t.Helper()
	images := 0
	for i, typ := range types {
		if typ != "image" {
			continue
		}
		images++
		if !strings.HasPrefix(media[i], "image/") {
			t.Errorf("image block %d carries %q; a file_ref must not become an image block", i, media[i])
		}
	}
	if images != tc.wantImages {
		t.Errorf("image blocks = %d, want %d (types %v)", images, tc.wantImages, types)
	}
	if len(texts) != 1 {
		t.Fatalf("text blocks = %d, want 1 (types %v)", len(texts), types)
	}
	got := texts[0]
	for _, want := range []string{"Read tool", fileRefPDF.WorkspacePath, "original name: " + fileRefPDF.OrigName} {
		if !strings.Contains(got, want) {
			t.Errorf("text block %q missing %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "]\n\n"+tc.text) {
		t.Errorf("text block %q does not end with hint + user text %q", got, tc.text)
	}
	if types[len(types)-1] != "text" {
		t.Errorf("text block is not last: %v", types)
	}
}

func TestACPProtocol_WriteMessage_FileRefBecomesReadHint(t *testing.T) {
	t.Parallel()
	for _, tc := range fileRefWireCases {
		t.Run(tc.name, func(t *testing.T) {
			p := &ACPProtocol{}
			p.storeSessionID("sess-test")
			var buf bytes.Buffer
			if err := p.WriteMessage(&buf, tc.text, tc.atts); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}
			var req struct {
				Params struct {
					Prompt []struct {
						Type   string `json:"type"`
						Text   string `json:"text"`
						Source *struct {
							MediaType string `json:"media_type"`
						} `json:"source"`
					} `json:"prompt"`
				} `json:"params"`
			}
			if err := json.Unmarshal(buf.Bytes(), &req); err != nil {
				t.Fatalf("decode: %v\n%s", err, buf.String())
			}
			var types, media, texts []string
			for _, b := range req.Params.Prompt {
				types = append(types, b.Type)
				mt := ""
				if b.Source != nil {
					mt = b.Source.MediaType
				}
				media = append(media, mt)
				if b.Type == "text" {
					texts = append(texts, b.Text)
				}
			}
			checkFileRefWire(t, tc, types, media, texts)
		})
	}
}

func TestCodexProtocol_WriteMessage_FileRefBecomesReadHint(t *testing.T) {
	t.Parallel()
	for _, tc := range fileRefWireCases {
		t.Run(tc.name, func(t *testing.T) {
			p := &CodexProtocol{}
			p.storeThreadID("t-1")
			var buf bytes.Buffer
			if err := p.WriteMessage(&buf, tc.text, tc.atts); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}
			var req struct {
				Params struct {
					Input []struct {
						Type     string `json:"type"`
						Text     string `json:"text"`
						ImageURL string `json:"image_url"`
					} `json:"input"`
				} `json:"params"`
			}
			if err := json.Unmarshal(buf.Bytes(), &req); err != nil {
				t.Fatalf("decode: %v\n%s", err, buf.String())
			}
			var types, media, texts []string
			for _, in := range req.Params.Input {
				types = append(types, in.Type)
				mt, _, _ := strings.Cut(strings.TrimPrefix(in.ImageURL, "data:"), ";")
				media = append(media, mt)
				if in.Type == "text" {
					texts = append(texts, in.Text)
				}
			}
			checkFileRefWire(t, tc, types, media, texts)
		})
	}
}
