package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
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

// TestReverseConn_ReadFileSegments: over a reverse conn no file_read answer
// carries more than ReverseFileChunk; a preview is reassembled from its
// segments and one that changes between them fails.
func TestReverseConn_ReadFileSegments(t *testing.T) {
	cases := []struct {
		name        string
		p           FileReadParams
		previewLen  int
		changeAfter int // answer requests past this one for another size; 0 never
		wantOffsets []int64
		wantLen     int
		wantErr     bool
	}{
		{name: "preview reassembled", p: FileReadParams{Mode: "preview"}, previewLen: 600 << 10, wantOffsets: []int64{0, 256 << 10, 512 << 10}, wantLen: 600 << 10},
		{name: "preview of whole chunks", p: FileReadParams{Mode: "preview"}, previewLen: 512 << 10, wantOffsets: []int64{0, 256 << 10, 512 << 10}, wantLen: 512 << 10},
		{name: "short preview", p: FileReadParams{Mode: "preview"}, previewLen: 5, wantOffsets: []int64{0}, wantLen: 5},
		{name: "preview changed", p: FileReadParams{Mode: "preview"}, previewLen: 600 << 10, changeAfter: 1, wantOffsets: []int64{0, 256 << 10}, wantErr: true},
		{name: "raw clamped", p: FileReadParams{Mode: "raw", Offset: 7, Limit: MaxFileChunk}, previewLen: 1 << 20, wantOffsets: []int64{7}, wantLen: ReverseFileChunk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, wsConn, cleanup := setupReverseConnPair(t)
			defer cleanup()
			rc.meta.Capabilities = map[string]bool{CapFiles: true}
			content := bytes.Repeat([]byte("a"), tc.previewLen)
			asked := make(chan FileReadParams, 8)
			go func() {
				for n := 1; ; n++ {
					var req ReverseMsg
					if err := wsConn.ReadJSON(&req); err != nil {
						return
					}
					var p FileReadParams
					_ = json.Unmarshal(req.Params, &p)
					asked <- p
					rd := FileRead{Size: int64(len(content)), Offset: p.Offset, Data: content[p.Offset:min(p.Offset+int64(p.Limit), int64(len(content)))]}
					if tc.changeAfter != 0 && n > tc.changeAfter {
						rd.Size++
					}
					res, _ := json.Marshal(rd)
					_ = wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: res})
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := rc.ReadFile(ctx, tc.p)
			if (err != nil) != tc.wantErr || len(got.Data) != tc.wantLen {
				t.Fatalf("ReadFile = %d bytes, %v; want %d bytes, err %v", len(got.Data), err, tc.wantLen, tc.wantErr)
			}
			close(asked)
			var offsets []int64
			for p := range asked {
				if p.Limit != ReverseFileChunk {
					t.Errorf("limit = %d, want %d", p.Limit, ReverseFileChunk)
				}
				offsets = append(offsets, p.Offset)
			}
			if !slices.Equal(offsets, tc.wantOffsets) {
				t.Errorf("offsets = %v, want %v", offsets, tc.wantOffsets)
			}
		})
	}
}
