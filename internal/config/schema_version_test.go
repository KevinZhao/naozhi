package config

import "testing"

// TestSchemaVersion_RejectsNewer verifies a config declaring a schema newer
// than the binary supports is rejected by validateConfig, so an older binary
// never runs a config written for a newer one.
func TestSchemaVersion_RejectsNewer(t *testing.T) {
	t.Parallel()
	cfg := &Config{SchemaVersion: CurrentSchemaVersion + 1}
	if err := validateConfig(cfg); err == nil {
		t.Fatalf("validateConfig accepted schema_version %d, want error", cfg.SchemaVersion)
	}
}

// TestSchemaVersion_AcceptsCurrent verifies the current version passes
// validation (no platforms configured is otherwise a valid minimal config).
func TestSchemaVersion_AcceptsCurrent(t *testing.T) {
	t.Parallel()
	cfg := &Config{SchemaVersion: CurrentSchemaVersion}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig rejected current schema_version: %v", err)
	}
}
