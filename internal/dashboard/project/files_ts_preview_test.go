package project

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleFileGet_PreviewTypeScriptIsText: .ts/.tsx map to
// application/typescript in previewableByExt, but that MIME was missing from
// textMimeSet so the preview endpoint flagged TypeScript sources as binary
// and the dashboard refused to render them.
func TestHandleFileGet_PreviewTypeScriptIsText(t *testing.T) {
	for _, name := range []string{"src/app.ts", "src/view.tsx"} {
		h, proj, _ := newProjectHandlersForTest(t, map[string]string{
			name: "export const x: number = 1;\n",
		})
		req := httptest.NewRequest(http.MethodGet,
			"/api/projects/file?project="+proj+"&path="+name+"&mode=preview", nil)
		w := httptest.NewRecorder()
		h.HandleFileGet(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body=%s", name, w.Code, w.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		if resp["binary"] != false {
			t.Errorf("%s: binary = %v, want false (mime=%v)", name, resp["binary"], resp["mime"])
		}
		if resp["content"] != "export const x: number = 1;\n" {
			t.Errorf("%s: content = %v", name, resp["content"])
		}
	}
}
