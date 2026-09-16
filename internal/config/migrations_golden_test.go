package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Golden tests for the v1 → v2 migration. They are byte-exact on purpose: the
// whole reason migrations operate on a yaml.Node document instead of
// round-tripping through Config is to keep the operator's comments, key order
// and formatting. A test that only checked "the key is gone" would pass for an
// implementation that reformatted the entire file and dropped every comment.

func migrateGolden(t *testing.T, in string) (string, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	res, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	return string(res.After), res.Applied
}

func TestMigrateV1ToV2_RenamesAndKeepsComments(t *testing.T) {
	in := `# naozhi config — hand maintained, comments matter
schema_version: 1
server:
  # the operator's own note about the port
  addr: ":8180"
nodes:
  laptop:
    url: "https://laptop.example.com"
session:
  # this comment belongs to the cwd key after the rename
  workspace: "/home/u/work"
  max_procs: 4
  auto_chain:
    enabled: true
    window_hours: 6
`
	want := `# naozhi config — hand maintained, comments matter
schema_version: 2
server:
  # the operator's own note about the port
  addr: ":8180"
workspaces:
  laptop:
    url: "https://laptop.example.com"
session:
  # this comment belongs to the cwd key after the rename
  cwd: "/home/u/work"
  max_procs: 4
`
	got, applied := migrateGolden(t, in)
	if got != want {
		t.Errorf("migrated document mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if len(applied) != 2 {
		t.Errorf("applied = %v, want one migration + the version bump", applied)
	}
}

func TestMigrateV1ToV2_LiftsAgentSystemPrompt(t *testing.T) {
	in := `schema_version: 1
agents:
  reviewer:
    model: "sonnet"
    # the flag is denylisted at spawn, so this never reached the CLI
    args: ["--append-system-prompt", "You review code."]
  planner:
    args: ["--verbose"]
`
	want := `schema_version: 2
agents:
  reviewer:
    model: "sonnet"
    # the flag is denylisted at spawn, so this never reached the CLI
    system_prompt: "You review code."
  planner:
    args: ["--verbose"]
`
	got, _ := migrateGolden(t, in)
	if got != want {
		t.Errorf("migrated document mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A file that already uses the modern keys must come out byte-identical apart
// from the version bump: the command must not become a reformatter.
func TestMigrateV1ToV2_CleanFileOnlyGainsTheVersion(t *testing.T) {
	in := `schema_version: 1
# a comment that must survive
workspaces:
  laptop:
    url: "https://laptop.example.com"
session:
  cwd: "/home/u/work"
agents:
  reviewer:
    system_prompt: "You review code."
`
	want := strings.Replace(in, "schema_version: 1", "schema_version: 2", 1)
	got, applied := migrateGolden(t, in)
	if got != want {
		t.Errorf("a clean file must only gain the version\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	for _, a := range applied {
		if !strings.HasPrefix(a, "schema_version:") {
			t.Errorf("nothing but the version should be reported for a clean file, got %q", a)
		}
	}
}

// Both spellings present: the modern key already holds the effective value (the
// load path prefers cwd over workspace and workspaces over nodes), so the
// deprecated one is dropped rather than overwriting it.
func TestMigrateV1ToV2_BothSpellingsKeepsTheModernValue(t *testing.T) {
	in := `schema_version: 1
session:
  cwd: "/the/real/one"
  workspace: "/the/deprecated/one"
`
	got, _ := migrateGolden(t, in)
	if !strings.Contains(got, `cwd: "/the/real/one"`) {
		t.Errorf("the modern value must survive:\n%s", got)
	}
	if strings.Contains(got, "workspace:") || strings.Contains(got, "/the/deprecated/one") {
		t.Errorf("the deprecated key and its value must be gone:\n%s", got)
	}
}

// The one ambiguous case refuses instead of guessing, matching the load path,
// which rejects the same conflict.
func TestMigrateV1ToV2_RefusesConflictingSystemPrompt(t *testing.T) {
	in := `schema_version: 1
agents:
  reviewer:
    system_prompt: "one thing"
    args: ["--append-system-prompt", "a different thing"]
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := MigrateFile(path)
	if err == nil {
		t.Fatal("a conflicting system_prompt must refuse rather than pick one")
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Errorf("error %q must name the agent whose config needs a human", err)
	}
}

// An unversioned file is treated as current by the load path, so the migration
// must not silently rewrite it for a migration it was never written for.
func TestMigrateFile_UnversionedIsLeftAlone(t *testing.T) {
	in := `session:
  workspace: "/home/u/work"
`
	got, applied := migrateGolden(t, in)
	if got != in {
		t.Errorf("an unversioned file must be left byte-identical\n--- got ---\n%s", got)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %v, want nothing", applied)
	}
}

// A file from a newer binary must be refused, not downgraded.
func TestMigrateFile_RefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 999\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := MigrateFile(path); err == nil {
		t.Fatal("a newer schema_version must be refused")
	}
}

// The produced document has to survive the real loader, not just a YAML parse.
// MigrateFile runs applyDefaults + parseDurations + validateConfig before
// handing bytes back, so a surgery bug cannot reach the disk.
func TestMigrateFile_ProducedDocumentLoads(t *testing.T) {
	in := `schema_version: 1
platforms:
  weixin:
    token: "wx-token"
nodes:
  laptop:
    url: "https://laptop.example.com"
session:
  workspace: "/home/u/work"
agents:
  reviewer:
    args: ["--append-system-prompt", "You review code."]
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	res, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if err := WriteMigrated(path, res); err != nil {
		t.Fatalf("WriteMigrated: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the migrated file must load: %v", err)
	}
	if cfg.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", cfg.SchemaVersion, CurrentSchemaVersion)
	}
	if cfg.Session.CWD != "/home/u/work" {
		t.Errorf("Session.CWD = %q, want the migrated value", cfg.Session.CWD)
	}
	if got := cfg.Agents["reviewer"].SystemPrompt; got != "You review code." {
		t.Errorf("agents[reviewer].SystemPrompt = %q, want the lifted value", got)
	}
	if args := cfg.Agents["reviewer"].Args; len(args) != 0 {
		t.Errorf("agents[reviewer].Args = %v, want empty after the lift", args)
	}
	// And a second run is a no-op: the file is now current.
	res2, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("second MigrateFile: %v", err)
	}
	if res2.Changed() {
		t.Errorf("migrating an already-migrated file must be a no-op, applied=%v", res2.Applied)
	}
}

// The re-validate step in MigrateFile is the guard that keeps a surgery bug off
// the disk: encode → re-parse → applyDefaults → parseDurations → validateConfig,
// and only then hand bytes back. Nothing exercised it, because the real v1 → v2
// migration cannot produce an invalid document. So this swaps a deliberately
// destructive migration into the chain.
//
// Not parallel: migrations is package-level state.
func TestMigrateFile_RefusesToProduceAnInvalidDocument(t *testing.T) {
	prev := migrations
	t.Cleanup(func() { migrations = prev })
	migrations = []migration{{
		From: 1,
		Desc: "test: write a node url the validator refuses",
		Apply: func(root *yaml.Node) (bool, error) {
			root.Content = append(root.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "nodes"},
				&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Value: "bad"},
					{Kind: yaml.MappingNode, Content: []*yaml.Node{
						{Kind: yaml.ScalarNode, Value: "url"},
						{Kind: yaml.ScalarNode, Value: "ftp://example.com"},
					}},
				}})
			return true, nil
		},
	}}

	path := filepath.Join(t.TempDir(), "config.yaml")
	before := "schema_version: 1\nserver:\n  addr: \":8180\"\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	res, err := MigrateFile(path)
	if err == nil {
		t.Fatal("MigrateFile must refuse a document its own migration made invalid")
	}
	if !strings.Contains(err.Error(), "validation") {
		t.Errorf("error %q must say the produced config failed validation", err)
	}
	// And the caller cannot be tricked into writing it: the result carries no
	// change, so WriteMigrated refuses too.
	if res.Changed() {
		t.Error("a refused migration must not report a change")
	}
	if err := WriteMigrated(path, res); err == nil {
		t.Error("WriteMigrated must refuse a result with no change")
	}
	if got, _ := os.ReadFile(path); string(got) != before {
		t.Errorf("the file must be untouched, got:\n%s", got)
	}
}
