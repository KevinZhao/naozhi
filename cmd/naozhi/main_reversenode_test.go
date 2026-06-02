package main

import (
	"testing"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/node"
)

// TestBuildReverseNodeAuth pins the cmd-boundary translation that lets
// internal/node stay free of an internal/config import (R040034-ARCH-1 /
// #1411). It verifies the field mapping (Token + DisplayName) and the
// nil-on-empty contract the caller's len()>0 guard relies on.
func TestBuildReverseNodeAuth(t *testing.T) {
	t.Parallel()

	if got := buildReverseNodeAuth(&config.Config{}); got != nil {
		t.Fatalf("empty reverse_nodes: got %#v, want nil", got)
	}

	cfg := &config.Config{
		ReverseNodes: map[string]config.ReverseNodeEntry{
			"node-a": {Token: "tok-a", DisplayName: "Node A"},
			"node-b": {Token: "tok-b"}, // no display name
		},
	}
	got := buildReverseNodeAuth(cfg)
	want := map[string]node.ReverseNodeAuth{
		"node-a": {Token: "tok-a", DisplayName: "Node A"},
		"node-b": {Token: "tok-b"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("missing node %q", id)
			continue
		}
		if g != w {
			t.Errorf("node %q = %#v, want %#v", id, g, w)
		}
	}
}
