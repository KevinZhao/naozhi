package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The re-validate step must see the document the way Load does — after ${VAR}
// expansion — while the bytes written back keep the placeholders. Validating
// the raw bytes tripped the containsEnvPlaceholder guards on every config that
// injects credentials from the environment (#2968).
//
// Not parallel: t.Setenv.
func TestMigrateFile_ExpandsEnvForValidationButWritesPlaceholders(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "cli_a")
	t.Setenv("FEISHU_APP_SECRET", "sec_b")
	in := `session:
  workspace: "/home/u/work"
platforms:
  feishu:
    app_id: "${FEISHU_APP_ID}"
    app_secret: "${FEISHU_APP_SECRET}"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	res, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none when every ${VAR} resolves", res.Warnings)
	}
	after := string(res.After)
	for _, ph := range []string{"${FEISHU_APP_ID}", "${FEISHU_APP_SECRET}"} {
		if !strings.Contains(after, ph) {
			t.Errorf("placeholder %s missing from the produced file:\n%s", ph, after)
		}
	}
	for _, secret := range []string{"cli_a", "sec_b"} {
		if strings.Contains(after, secret) {
			t.Errorf("expanded value %q leaked into the produced file:\n%s", secret, after)
		}
	}
	if !strings.Contains(after, "cwd:") {
		t.Errorf("migration did not run:\n%s", after)
	}
}

// An unset ${VAR} is the operator's shell lacking the service's EnvironmentFile,
// not a defect the migration introduced. The failure is shared by the original
// document, so it is reported as a warning and the migration still goes through.
//
// Not parallel: t.Setenv.
func TestMigrateFile_UnsetEnvIsAWarningNotAFatal(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "")
	os.Unsetenv("FEISHU_APP_ID")
	t.Setenv("FEISHU_APP_SECRET", "")
	os.Unsetenv("FEISHU_APP_SECRET")
	in := `session:
  workspace: "/home/u/work"
platforms:
  feishu:
    app_id: "${FEISHU_APP_ID}"
    app_secret: "${FEISHU_APP_SECRET}"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	res, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile must not refuse a pre-existing validation failure: %v", err)
	}
	if !res.Changed() {
		t.Fatal("migration must still apply")
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "unexpanded ${VAR}") {
		t.Errorf("Warnings = %q, want one naming the unexpanded placeholder", res.Warnings)
	}
	if !strings.Contains(string(res.After), "${FEISHU_APP_ID}") {
		t.Errorf("placeholder must survive:\n%s", res.After)
	}
}

// The repository's own template is the shape the README recommends; migrate
// has to accept it.
func TestMigrateFile_AcceptsConfigExampleYAML(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Skipf("config.example.yaml not readable from the package dir: %v", err)
	}
	// Drop the version line so the chain runs and the re-validate step is
	// reached; an already-current file returns before it.
	var kept []string
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "schema_version:") {
			continue
		}
		kept = append(kept, line)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	res, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile(config.example.yaml): %v", err)
	}
	if !res.Changed() {
		t.Fatal("the unversioned template must gain schema_version")
	}
	if !strings.Contains(string(res.After), "${FEISHU_APP_ID}") {
		t.Errorf("placeholders must survive the rewrite")
	}
}
