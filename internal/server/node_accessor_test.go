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

// TestLookupNode_JSONEnvelope pins R20260531A-ARCH-5: LookupNode must return
// a JSON error envelope (via errResp) rather than text/plain (http.Error) so
// all API error responses are consistent. Asserts Content-Type, status code,
// and the presence of the closed-vocabulary "code" field for each error path.
func TestLookupNode_JSONEnvelope(t *testing.T) {
	t.Parallel()

	mu := &sync.RWMutex{}
	nodes := map[string]node.Conn{
		"known-node": nil,
	}
	a := newNodeAccessor(mu, nodes, map[string]string{"known-node": "Known"})

	cases := []struct {
		name       string
		id         string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "id_too_long",
			id:         strings.Repeat("a", maxNodeIDBytes+1),
			wantStatus: http.StatusBadRequest,
			wantCode:   "node_id_too_long",
		},
		{
			name:       "invalid_characters",
			id:         "bad\x00node",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_node_id",
		},
		{
			name:       "unknown_node",
			id:         "not-registered",
			wantStatus: http.StatusBadRequest,
			wantCode:   "unknown_node",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			nc, ok := a.LookupNode(w, tc.id)
			if ok || nc != nil {
				t.Fatalf("LookupNode returned ok=true — must return false for %s", tc.name)
			}
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}

			ct := w.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json prefix (not text/plain)", ct)
			}

			var env struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not valid JSON: %v (body=%s)", err, w.Body.String())
			}
			if env.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", env.Code, tc.wantCode)
			}
		})
	}
}

// TestLookupNode_HappyPath verifies the non-error path still works.
func TestLookupNode_HappyPath(t *testing.T) {
	t.Parallel()

	mu := &sync.RWMutex{}
	nodes := map[string]node.Conn{
		"my-node": nil,
	}
	a := newNodeAccessor(mu, nodes, map[string]string{"my-node": "My Node"})

	w := httptest.NewRecorder()
	nc, ok := a.LookupNode(w, "my-node")
	if !ok {
		t.Fatal("LookupNode returned ok=false for a registered node")
	}
	_ = nc
	if w.Code != http.StatusOK {
		t.Fatalf("happy path wrote status %d, want 200 (no error response written)", w.Code)
	}
}
