package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestACPProtocol_WriteMessage_TypedParamsByteEqual locks the wire format
// produced by the typed acpPromptParams / acpPromptBlock structs (R228-PERF-4)
// against the previous map[string]any literal. If the JSON keys, key order,
// or omitempty semantics ever drift, this test fails and forces a wire-protocol
// review against ACP / kiro spec.
func TestACPProtocol_WriteMessage_TypedParamsByteEqual(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		text   string
		images []ImageData
		// expectPromptBlocks lists the EXACT content blocks the wire frame
		// must carry, in order. Each block is the JSON that should land
		// inside the "prompt" array.
		expectPromptBlocks []string
	}{
		{
			name:   "text_only",
			text:   "hello",
			images: nil,
			expectPromptBlocks: []string{
				`{"type":"text","text":"hello"}`,
			},
		},
		{
			name:   "empty_text_no_images_still_emits_text_block",
			text:   "",
			images: nil,
			expectPromptBlocks: []string{
				// Empty text: keeps the "text" key (byte-equal to the
				// prior map[string]string literal) so the kiro server
				// sees the same wire shape.
				`{"type":"text","text":""}`,
			},
		},
		{
			name: "single_image_no_text",
			text: "",
			images: []ImageData{
				{MimeType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
			},
			expectPromptBlocks: []string{
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
					base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4e, 0x47}) + `"}}`,
			},
		},
		{
			name: "single_image_with_text",
			text: "describe",
			images: []ImageData{
				{MimeType: "image/jpeg", Data: []byte("AB")},
			},
			expectPromptBlocks: []string{
				`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` +
					base64.StdEncoding.EncodeToString([]byte("AB")) + `"}}`,
				`{"type":"text","text":"describe"}`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &ACPProtocol{}
			// Set sessionID to a known value so the wire frame is stable.
			p.storeSessionID("sess-test")

			var buf bytes.Buffer
			if err := p.WriteMessage(&buf, tc.text, tc.images); err != nil {
				t.Fatalf("WriteMessage err: %v", err)
			}
			out := strings.TrimRight(buf.String(), "\n")

			// Top-level frame must be a JSON-RPC request with method
			// session/prompt. Decoding into a generic map confirms the
			// outer envelope without locking ID generation.
			var envelope map[string]any
			if err := json.Unmarshal([]byte(out), &envelope); err != nil {
				t.Fatalf("non-JSON output: %v\nout=%s", err, out)
			}
			if envelope["jsonrpc"] != "2.0" {
				t.Errorf("jsonrpc = %v, want 2.0", envelope["jsonrpc"])
			}
			if envelope["method"] != "session/prompt" {
				t.Errorf("method = %v, want session/prompt", envelope["method"])
			}

			params, ok := envelope["params"].(map[string]any)
			if !ok {
				t.Fatalf("params not an object: %#v", envelope["params"])
			}
			if params["sessionId"] != "sess-test" {
				t.Errorf("sessionId = %v, want sess-test", params["sessionId"])
			}

			// Now byte-equal-check the "prompt" sub-frame. Re-marshal
			// each block from the decoded form so map key ordering is
			// canonical, then compare against the expected strings.
			rawPrompt, ok := params["prompt"].([]any)
			if !ok {
				t.Fatalf("prompt not an array: %#v", params["prompt"])
			}
			if len(rawPrompt) != len(tc.expectPromptBlocks) {
				t.Fatalf("prompt block count = %d, want %d (got %#v)",
					len(rawPrompt), len(tc.expectPromptBlocks), rawPrompt)
			}
			for i, blk := range rawPrompt {
				gotBytes, err := json.Marshal(blk)
				if err != nil {
					t.Fatalf("re-marshal block %d: %v", i, err)
				}
				want := tc.expectPromptBlocks[i]
				// Re-marshal expected too so map key ordering matches.
				var wantParsed any
				if err := json.Unmarshal([]byte(want), &wantParsed); err != nil {
					t.Fatalf("test bug: parse expect[%d]: %v", i, err)
				}
				wantCanon, _ := json.Marshal(wantParsed)
				if !bytes.Equal(gotBytes, wantCanon) {
					t.Errorf("block %d:\n got:  %s\n want: %s", i, gotBytes, wantCanon)
				}
			}
		})
	}
}
