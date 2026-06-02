package main

import (
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

// TestBuildRemoteNodes covers the cfg.Nodes → node.Conn map construction
// extracted from main() in R237-ARCH-8 (#590): no nodes yields nil (so the
// server's nil/empty equivalence holds and the "multi-node configured" log
// stays silent), and each configured node produces exactly one client keyed
// by its id.
func TestBuildRemoteNodes(t *testing.T) {
	t.Parallel()

	if got := buildRemoteNodes(&config.Config{}); got != nil {
		t.Fatalf("no nodes: buildRemoteNodes = %v, want nil", got)
	}

	cfg := &config.Config{
		Nodes: map[string]config.NodeConfig{
			"alpha": {URL: "https://a.example", Token: "t1", DisplayName: "Alpha"},
			"beta":  {URL: "https://b.example", Token: "t2"},
		},
	}
	nodes := buildRemoteNodes(cfg)
	if len(nodes) != 2 {
		t.Fatalf("buildRemoteNodes = %d entries, want 2", len(nodes))
	}
	for _, id := range []string{"alpha", "beta"} {
		if nodes[id] == nil {
			t.Errorf("nodes[%q] is nil, want a constructed client", id)
		}
	}
	if _, ok := nodes["gamma"]; ok {
		t.Errorf("nodes contains unexpected key gamma")
	}
}
