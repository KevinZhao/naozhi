package node

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestReverseConn_FileRPCs: a node without CapFiles is never asked; one that
// advertises it gets the RPC, and an "unknown method" answer (a node whose
// binary predates the method) reads as ErrFilesUnsupported too.
func TestReverseConn_FileRPCs(t *testing.T) {
	cases := []struct {
		name    string
		caps    map[string]bool
		answer  ReverseMsg // the node's response; Type "" sends none
		wantErr error
		wantDir string
	}{
		{name: "no cap", wantErr: ErrFilesUnsupported},
		{name: "unknown method", caps: map[string]bool{CapFiles: true}, answer: ReverseMsg{Type: "response", Error: `unknown method: "files_list"`}, wantErr: ErrFilesUnsupported},
		{name: "served", caps: map[string]bool{CapFiles: true}, answer: ReverseMsg{Type: "response", Result: []byte(`{"dir":"sub","entries":[]}`)}, wantDir: "sub"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, wsConn, cleanup := setupReverseConnPair(t)
			defer cleanup()
			rc.meta.Capabilities = tc.caps
			asked := make(chan ReverseMsg, 1)
			go func() {
				_ = wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
				var req ReverseMsg
				if err := wsConn.ReadJSON(&req); err != nil {
					return
				}
				asked <- req
				ans := tc.answer
				ans.ReqID = req.ReqID
				_ = wsConn.WriteJSON(ans)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			list, err := rc.ListFiles(ctx, FilesListParams{FileRoot: FileRoot{Key: "k"}, Dir: "sub"})
			if !errors.Is(err, tc.wantErr) || list.Dir != tc.wantDir {
				t.Fatalf("ListFiles = %+v, %v; want dir %q, err %v", list, err, tc.wantDir, tc.wantErr)
			}
			select {
			case req := <-asked:
				if tc.caps == nil {
					t.Fatalf("a node without %s was asked %s", CapFiles, req.Method)
				}
				if req.Method != "files_list" {
					t.Fatalf("method = %q", req.Method)
				}
			default:
				if tc.caps != nil {
					t.Fatal("the node was never asked")
				}
			}
		})
	}
}
