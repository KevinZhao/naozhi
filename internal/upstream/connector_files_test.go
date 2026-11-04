package upstream

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// TestFileRPCs: the file RPCs reach the wired Files, decoding their params;
// an unwired node neither advertises node.CapFiles nor serves them, and
// answers as an older binary does.
func TestFileRPCs(t *testing.T) {
	wired := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, Discovery{}, nil)
	wired.ServeFiles(Files{
		List: func(p node.FilesListParams) node.FileList {
			return node.FileList{Dir: p.Key + "|" + p.Dir, Entries: []node.FileEntry{}}
		},
		Read: func(p node.FileReadParams) node.FileRead {
			return node.FileRead{Size: p.Offset, Data: []byte(p.Path + "|" + p.Mode)}
		},
		Exists: func(p node.FilesExistsParams) node.FilesExists {
			return node.FilesExists{Results: map[string]node.FileStat{p.Paths[0]: {Exists: true}}}
		},
		Outputs: func(key string) any { return map[string]string{"key": key} },
	})
	bare := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, Discovery{}, nil)

	if !slices.Contains(wired.registerCaps(), node.CapFiles) || slices.Contains(bare.registerCaps(), node.CapFiles) {
		t.Fatalf("caps: wired %v, bare %v", wired.registerCaps(), bare.registerCaps())
	}

	const key = "feishu:direct:alice:general"
	cases := []struct {
		method string
		params any
		want   string // substring of the result, or of the error with wantErr
		err    bool
	}{
		{"files_list", node.FilesListParams{FileRoot: node.FileRoot{Key: key}, Dir: "sub"}, `"dir":"` + key + `|sub"`, false},
		{"file_read", node.FileReadParams{Path: "a.txt", Mode: "raw", Offset: 7}, `"size":7`, false},
		{"files_exists", node.FilesExistsParams{Project: "p", Paths: []string{"x"}}, `"x":{"exists":true}`, false},
		{"fetch_outputs", map[string]string{"key": key}, `"key":"` + key + `"`, false},
		{"fetch_outputs", map[string]string{"key": "bad\nkey"}, "fetch_outputs key", true},
		{"file_read", "not an object", "file_read params", true},
	}
	for _, tc := range cases {
		params, _ := json.Marshal(tc.params)
		req := node.ReverseMsg{Method: tc.method, Params: params}
		res, err := wired.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
		if tc.err {
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: err = %v, want %q", tc.method, err, tc.want)
			}
		} else if err != nil || !strings.Contains(string(res), tc.want) {
			t.Errorf("%s: %s, %v; want %q", tc.method, res, err, tc.want)
		}

		if _, err := bare.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{}); err == nil || !strings.Contains(err.Error(), "unknown method") {
			t.Errorf("%s on an unwired node: err = %v, want unknown method", tc.method, err)
		}
	}
}
