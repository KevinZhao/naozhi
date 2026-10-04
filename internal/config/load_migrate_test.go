package config

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/spawndiag"
)

// legacyConfig carries every v1 shape the migration chain rewrites.
const legacyConfig = `platforms:
  weixin:
    token: "x"
nodes:
  a:
    url: "https://a.example"
session:
  workspace: "/home/u/work"
  auto_chain:
    enabled: true
agents:
  reviewer:
    model: sonnet
    args: ["--append-system-prompt", "Review carefully.", "--keep"]
  bare:
    args: ["--keep", "--append-system-prompt"]
`

func deprecatedDiags(diags []spawndiag.Diag) []string {
	var out []string
	for _, d := range diags {
		if d.Layer == "config-deprecated" {
			out = append(out, d.Key+"="+d.Action)
		}
	}
	slices.Sort(out)
	return out
}

// Load reports each rewrite the chain makes, with the key and action the
// dashboard and `config check` have always shown, and no unknown-key diag for
// the keys Config no longer has a field for.
func TestLoad_ReportsEachMigratedKey(t *testing.T) {
	diags, _, err := collectLoadDiags(t, writeCfg(t, legacyConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{
		"agents[bare].args=dropped",
		"agents[reviewer].args=rewritten",
		"nodes=rewritten",
		"session.auto_chain=ignored",
		"session.workspace=rewritten",
	}
	if got := deprecatedDiags(diags); !slices.Equal(got, want) {
		t.Errorf("deprecated diags = %v, want %v", got, want)
	}
	if u := unknownDiags(diags); len(u) != 0 {
		t.Errorf("a migrated key was also reported unknown: %+v", u)
	}
}

// Loading a legacy file yields exactly the Config its migrated file does, and
// leaves the file's bytes alone.
func TestLoad_LegacyEqualsMigrated(t *testing.T) {
	path := writeCfg(t, legacyConfig)
	legacy, err := Load(path)
	if err != nil {
		t.Fatalf("Load legacy: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != legacyConfig {
		t.Errorf("Load changed the file:\n%s", got)
	}
	migrated, _ := migrateGolden(t, legacyConfig)
	modern, err := Load(writeCfg(t, migrated))
	if err != nil {
		t.Fatalf("Load migrated: %v", err)
	}
	legacy.Fingerprint, modern.Fingerprint = Fingerprint{}, Fingerprint{}
	if !reflect.DeepEqual(legacy, modern) {
		t.Errorf("legacy and migrated files load differently:\nlegacy: %+v\nmodern: %+v", legacy, modern)
	}
	if legacy.Session.CWD != "/home/u/work" || legacy.Nodes["a"].URL != "https://a.example" ||
		legacy.Agents["reviewer"].SystemPrompt != "Review carefully." || !slices.Equal(legacy.Agents["bare"].Args, []string{"--keep"}) {
		t.Errorf("legacy values were not carried over: %+v", legacy)
	}
}

// When both spellings are present the modern one wins and the old is reported
// as ignored.
func TestLoad_BothSpellingsKeepsTheModernValue(t *testing.T) {
	body := `platforms:
  weixin:
    token: "x"
nodes:
  old:
    url: "https://old.example"
workspaces:
  new:
    url: "https://new.example"
`
	diags, cfg, err := collectLoadDiags(t, writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := deprecatedDiags(diags); !slices.Equal(got, []string{"nodes=ignored"}) {
		t.Errorf("deprecated diags = %v, want [nodes=ignored]", got)
	}
	if _, ok := cfg.Nodes["new"]; !ok || len(cfg.Nodes) != 1 {
		t.Errorf("Nodes = %v, want only the workspaces entry", cfg.Nodes)
	}
}

// A file that declares the current schema but still carries a v1 key is
// migrated too: the key keeps working and is reported deprecated (#2897 D10),
// and config migrate rewrites it.
func TestLoad_CurrentSchemaStillMigratesV1Keys(t *testing.T) {
	body := "schema_version: 2\nplatforms:\n  weixin:\n    token: \"x\"\nnodes:\n  a:\n    url: \"https://a.example\"\n"
	diags, cfg, err := collectLoadDiags(t, writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := deprecatedDiags(diags); !slices.Equal(got, []string{"nodes=rewritten"}) {
		t.Errorf("deprecated diags = %v, want [nodes=rewritten]", got)
	}
	if u := unknownDiags(diags); len(u) != 0 {
		t.Errorf("unknown diags = %+v, want none", u)
	}
	if cfg.Nodes["a"].URL != "https://a.example" {
		t.Errorf("Nodes = %v, want the v1 entry", cfg.Nodes)
	}
	migrated, applied := migrateGolden(t, body)
	if len(applied) != 1 || !strings.Contains(migrated, "workspaces:") || strings.Contains(migrated, "nodes:") {
		t.Errorf("migrate on a v2 file with a v1 key: applied=%v\n%s", applied, migrated)
	}
}

// A file newer than this binary is not migrated: its shape is not one the
// chain knows, and validateConfig rejects it.
func TestLoad_NewerSchemaIsNotMigrated(t *testing.T) {
	diags, _, err := collectLoadDiags(t, writeCfg(t, "schema_version: 9\nnodes:\n  a:\n    url: \"https://a.example\"\n"))
	if err == nil || !strings.Contains(err.Error(), "upgrade naozhi") {
		t.Fatalf("Load error = %v, want the newer-schema rejection", err)
	}
	if got := deprecatedDiags(diags); len(got) != 0 {
		t.Errorf("deprecated diags = %v, want none for a schema this binary does not know", got)
	}
}

func TestLoad_MigrationErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, want, notWant string
	}{
		{"newer schema", "schema_version: 9\n", "upgrade naozhi", ""},
		{"schema version not an integer", "schema_version: s3cr3t\n", "not an integer", "s3cr3t"},
		{"args not strings", "agents:\n  r:\n    args: [{k: v}, \"--append-system-prompt\"]\n", "agents[r].args", ""},
		{"conflicting system prompt", "agents:\n  r:\n    system_prompt: explicit\n    args: [\"--append-system-prompt\", \"legacy\"]\n", "agents[r]", ""},
		{"quoted null is a string", "agents:\n  r:\n    system_prompt: \"null\"\n    args: [\"--append-system-prompt\", \"legacy\"]\n", "agents[r]", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want one containing %q", err, tc.want)
			}
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("Load error echoes the value: %v", err)
			}
		})
	}
}

// system_prompt equal to the flag's value is not a conflict: both say the
// same thing.
func TestLoad_EqualSystemPromptIsNotAConflict(t *testing.T) {
	body := "platforms:\n  weixin:\n    token: \"x\"\nagents:\n  r:\n    system_prompt: same\n    args: [\"--append-system-prompt\", \"same\"]\n"
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a := cfg.Agents["r"]; a.SystemPrompt != "same" || len(a.Args) != 0 {
		t.Errorf("agent = %+v, want the prompt kept and the flag gone", a)
	}
}

// A YAML null system_prompt is an empty placeholder: the flag's text lifts
// into it instead of counting as a conflict or leaving a !!null node that
// cannot decode into a string.
func TestLoad_NullSystemPromptTakesTheLiftedFlag(t *testing.T) {
	for _, null := range []string{"", " ~", " null", " Null", " !!null"} {
		t.Run(null, func(t *testing.T) {
			body := "platforms:\n  weixin:\n    token: \"x\"\nagents:\n  planner:\n    system_prompt:" + null + "\n    args: [\"--append-system-prompt\", \"hello\"]\n"
			diags, cfg, err := collectLoadDiags(t, writeCfg(t, body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if a := cfg.Agents["planner"]; a.SystemPrompt != "hello" || len(a.Args) != 0 {
				t.Errorf("agent = %+v, want the flag lifted into system_prompt", a)
			}
			if got, want := deprecatedDiags(diags), []string{"agents[planner].args=rewritten"}; !slices.Equal(got, want) {
				t.Errorf("deprecated diags = %v, want %v", got, want)
			}
		})
	}
}

// An empty file decodes to the zero Config, as it did before Load parsed into
// a node first.
func TestLoad_EmptyFile(t *testing.T) {
	cfg, err := Load(writeCfg(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SchemaVersion != CurrentSchemaVersion || cfg.Session.CWD != defaultSessionCWD {
		t.Errorf("empty file = %+v, want defaults only", cfg)
	}
}
