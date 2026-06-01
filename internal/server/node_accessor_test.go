package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// TestLookupNode_JSONErrorEnvelope pins #451 / R247-ARCH-3: LookupNode's
// three rejection paths (too-long / invalid-charset / unknown) now reply
// with the unified errEnvelope JSON shape carrying a stable machine code,
// instead of text/plain http.Error. Every caller is a dashboard JSON API
// handler whose front-end reads body.error, so a plain-text reply forced
// the UI to branch on Content-Type.
func TestLookupNode_JSONErrorEnvelope(t *testing.T) {
	t.Parallel()

	acc := newNodeAccessor(&sync.RWMutex{}, map[string]node.Conn{}, map[string]string{})

	cases := []struct {
		name     string
		id       string
		wantCode string
	}{
		{"too long", strings.Repeat("a", maxNodeIDBytes+1), "node_id_too_long"},
		{"invalid charset", "bad id!", "node_id_invalid"},
		{"unknown node", "ghost", "node_unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			nc, ok := acc.LookupNode(w, tc.id)
			if ok || nc != nil {
				t.Fatalf("LookupNode(%q) should reject, got ok=%v nc=%v", tc.id, ok, nc)
			}
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var env errEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not JSON: %v (body=%q)", err, w.Body.String())
			}
			if env.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Code, tc.wantCode)
			}
			if env.Error == "" {
				t.Error("error message should be non-empty")
			}
		})
	}
}
