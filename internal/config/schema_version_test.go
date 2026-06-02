package config

import "testing"

// TestSchemaVersion_DefaultsWhenAbsent verifies a pre-versioning config (no
// schema_version key → 0) is normalized to CurrentSchemaVersion so existing
// deployments load unchanged. R243-ARCH-14 / #843.
func TestSchemaVersion_DefaultsWhenAbsent(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	applyDefaults(cfg)
	if cfg.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d (default for absent key)", cfg.SchemaVersion, CurrentSchemaVersion)
	}
}

// TestSchemaVersion_ExplicitPreserved verifies an explicitly-set, supported
// version is not overwritten by applyDefaults.
func TestSchemaVersion_ExplicitPreserved(t *testing.T) {
	t.Parallel()
	cfg := &Config{SchemaVersion: CurrentSchemaVersion}
	applyDefaults(cfg)
	if cfg.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d (explicit preserved)", cfg.SchemaVersion, CurrentSchemaVersion)
	}
}

// TestSchemaVersion_RejectsNewer verifies a config declaring a schema newer
// than the binary supports is rejected by validateConfig — the hook the
// config/v1 migration entry will build on.
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
