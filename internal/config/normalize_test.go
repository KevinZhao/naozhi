package config

import (
	"testing"
)

// TestNormalize_WorkspacesOnly covers the preferred YAML spelling path:
// an operator who only writes `workspaces:` gets cfg.Nodes populated too,
// so every downstream consumer (validateConfig, main.go) sees the entries.
func TestNormalize_WorkspacesOnly(t *testing.T) {
	cfg := &Config{
		Workspaces: map[string]NodeConfig{
			"macbook": {URL: "https://10.0.0.2:8180", Token: "t"},
		},
	}
	cfg.Normalize()
	if got := len(cfg.Nodes); got != 1 {
		t.Fatalf("Nodes len = %d, want 1 (promoted from Workspaces)", got)
	}
	if cfg.Nodes["macbook"].URL != "https://10.0.0.2:8180" {
		t.Errorf("Nodes[macbook].URL = %q, want https://10.0.0.2:8180", cfg.Nodes["macbook"].URL)
	}
}

// TestNormalize_NodesOnly covers the legacy spelling path: both sides are
// populated so code that migrated to read Workspaces can also find entries.
func TestNormalize_NodesOnly(t *testing.T) {
	cfg := &Config{
		Nodes: map[string]NodeConfig{
			"old": {URL: "https://host:8180"},
		},
	}
	cfg.Normalize()
	if got := len(cfg.Workspaces); got != 1 {
		t.Fatalf("Workspaces len = %d, want 1 (promoted from Nodes)", got)
	}
	if cfg.Workspaces["old"].URL != "https://host:8180" {
		t.Errorf("Workspaces[old].URL = %q", cfg.Workspaces["old"].URL)
	}
}

// TestNormalize_BothSet verifies the conflict resolution: Workspaces wins
// and overwrites Nodes, matching the semantic "workspaces is preferred".
func TestNormalize_BothSet(t *testing.T) {
	cfg := &Config{
		Nodes: map[string]NodeConfig{
			"n1": {URL: "https://nodes-variant:1"},
		},
		Workspaces: map[string]NodeConfig{
			"w1": {URL: "https://workspaces-variant:1"},
		},
	}
	cfg.Normalize()
	if _, ok := cfg.Nodes["w1"]; !ok {
		t.Errorf("Nodes should contain w1 after Workspaces wins, got %v", cfg.Nodes)
	}
	if _, ok := cfg.Nodes["n1"]; ok {
		t.Errorf("Nodes should NOT contain n1 after Workspaces wins, got %v", cfg.Nodes)
	}
}

// TestNormalize_Idempotent ensures calling Normalize twice is safe — the
// second call sees both maps equal and must not drop or duplicate entries.
func TestNormalize_Idempotent(t *testing.T) {
	cfg := &Config{
		Workspaces: map[string]NodeConfig{
			"x": {URL: "https://x:8180"},
		},
	}
	cfg.Normalize()
	cfg.Normalize()
	if got := len(cfg.Nodes); got != 1 {
		t.Errorf("Nodes len after two Normalize calls = %d, want 1", got)
	}
	if got := len(cfg.Workspaces); got != 1 {
		t.Errorf("Workspaces len after two Normalize calls = %d, want 1", got)
	}
}

// TestNormalize_Empty covers the zero-nodes deployment: Normalize must not
// panic or fabricate entries when both maps are nil.
func TestNormalize_Empty(t *testing.T) {
	cfg := &Config{}
	cfg.Normalize()
	if cfg.Nodes != nil && len(cfg.Nodes) != 0 {
		t.Errorf("Nodes should remain nil/empty, got %v", cfg.Nodes)
	}
	if cfg.Workspaces != nil && len(cfg.Workspaces) != 0 {
		t.Errorf("Workspaces should remain nil/empty, got %v", cfg.Workspaces)
	}
}
