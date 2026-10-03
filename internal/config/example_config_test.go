package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// readExampleRoot parses the shipped config.example.yaml and returns its root
// mapping.
func readExampleRoot(t *testing.T) *yaml.Node {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse config.example.yaml: %v", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		t.Fatal("config.example.yaml has no root mapping")
	}
	return doc.Content[0]
}

// TestExampleConfig_NeedsNoMigration: an operator copies the template, so any
// deprecated form left in it (the legacy agents[].args --append-system-prompt,
// `nodes:`, `session.workspace`) prints a config-deprecated WARN on first boot.
func TestExampleConfig_NeedsNoMigration(t *testing.T) {
	_, changes, err := runMigrations(readExampleRoot(t))
	if err != nil {
		t.Fatalf("runMigrations(config.example.yaml): %v", err)
	}
	for _, c := range changes {
		t.Errorf("config.example.yaml still uses a deprecated form: %s %s (%s)", c.Key, c.Action, c.Reason)
	}
}

// TestExampleConfig_TrustedProxyOff: a copied trusted_proxy: true refuses every
// direct LAN login with 400, so the template keeps the key visible and false,
// the same as the struct default.
func TestExampleConfig_TrustedProxyOff(t *testing.T) {
	server := yamlChildMap(readExampleRoot(t), "server")
	if server == nil {
		t.Fatal("config.example.yaml has no server block")
	}
	tp := yamlChildScalar(server, "trusted_proxy")
	if tp == nil {
		t.Fatal("config.example.yaml must document server.trusted_proxy")
	}
	var on bool
	if err := tp.Decode(&on); err != nil {
		t.Fatalf("server.trusted_proxy: %v", err)
	}
	if on {
		t.Error("config.example.yaml sets server.trusted_proxy: true; the template must default to false")
	}
}

// yamlChildScalar returns the scalar node for key, or nil.
func yamlChildScalar(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.ScalarNode {
			return m.Content[i+1]
		}
	}
	return nil
}
