package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// writeCfg writes body to a temp config.yaml and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// collectLoadDiags loads path with a diag observer installed and returns the
// diags Load emitted, the config and the error.
func collectLoadDiags(t *testing.T, path string) ([]cli.SpawnDiag, *Config, error) {
	t.Helper()
	var got []cli.SpawnDiag
	restore := cli.ObserveSpawnDiags(func(scope string, d cli.SpawnDiag) {
		if scope != "config" {
			t.Errorf("diag scope = %q, want \"config\"", scope)
		}
		got = append(got, d)
	})
	defer restore()
	cfg, err := Load(path)
	return got, cfg, err
}

func unknownDiags(in []cli.SpawnDiag) []cli.SpawnDiag {
	var out []cli.SpawnDiag
	for _, d := range in {
		if d.Layer == "config-unknown" {
			out = append(out, d)
		}
	}
	return out
}

// TestUnknownKeysReported is the regression this file exists for: a misspelled
// or non-existent key used to load with no error, no effect and no diagnostic
// (#2639). Every case names the dotted path, because the yaml decoder reports
// only the leaf and this schema reuses leaf names under many parents.
func TestUnknownKeysReported(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		paths []string
	}{
		{
			name:  "top_level",
			body:  "totally_made_up: 1\n",
			paths: []string{"totally_made_up"},
		},
		{
			name:  "nested_reports_dotted_path_not_bare_leaf",
			body:  "server:\n  debug_moed: true\n",
			paths: []string{"server.debug_moed"},
		},
		{
			name: "the_two_keys_from_2553_spelled_wrong",
			body: "server:\n  debug_moed: true\nprojects:\n  publc_tmp: true\n",
			// Both wrong spellings are reported; the correct spellings are
			// covered by TestKnownKeysAreNotReported below.
			paths: []string{"server.debug_moed", "projects.publc_tmp"},
		},
		{
			name:  "inside_a_sequence_element",
			body:  "cli:\n  backends:\n    - id: claude\n      pahs: /usr/bin/claude\n",
			paths: []string{"cli.backends[0].pahs"},
		},
		{
			name:  "same_leaf_name_under_two_parents_is_disambiguated",
			body:  "server:\n  bogus: 1\nprojects:\n  bogus: 2\n",
			paths: []string{"server.bogus", "projects.bogus"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags, cfg, err := collectLoadDiags(t, writeCfg(t, tc.body))
			if err != nil {
				t.Fatalf("Load must not fail on an unknown key: %v", err)
			}
			if cfg == nil {
				t.Fatal("Load returned a nil config")
			}
			got := unknownDiags(diags)
			var paths []string
			for _, d := range got {
				paths = append(paths, d.Key)
				if d.Action != "ignored" {
					t.Errorf("action = %q, want \"ignored\"", d.Action)
				}
				if !strings.Contains(d.Reason, "line ") {
					t.Errorf("reason must carry the line number, got %q", d.Reason)
				}
			}
			for _, want := range tc.paths {
				found := false
				for _, p := range paths {
					if p == want {
						found = true
					}
				}
				if !found {
					t.Errorf("missing unknown key %q; got %v", want, paths)
				}
			}
			if len(paths) != len(tc.paths) {
				t.Errorf("got %d unknown keys %v, want %d %v", len(paths), paths, len(tc.paths), tc.paths)
			}
		})
	}
}

// TestKnownKeysAreNotReported is the false-positive side, and it is the half
// that matters operationally: every real key in a live config.yaml must stay
// silent, or the report is noise an operator learns to ignore. The two keys
// wired in #2628 are here by name because they are the ones this whole thread
// started from.
func TestKnownKeysAreNotReported(t *testing.T) {
	body := `server:
  debug_mode: true
  dashboard_token: "t"
projects:
  public_tmp: true
  include_root: true
session:
  cwd: /tmp
cli:
  backends:
    - id: claude
      path: /usr/bin/claude
platforms:
  weixin:
    token: "wx"
`
	diags, cfg, err := collectLoadDiags(t, writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := unknownDiags(diags); len(got) != 0 {
		t.Errorf("real keys reported as unknown: %+v", got)
	}
	// And they actually landed, so this is not passing because the keys were
	// dropped some other way.
	if !cfg.Server.DebugMode || !cfg.Projects.PublicTmp {
		t.Errorf("DebugMode = %v, PublicTmp = %v; want both true", cfg.Server.DebugMode, cfg.Projects.PublicTmp)
	}
}

// TestExampleConfigHasNoUnknownKeys checks the shipped config.example.yaml
// against the struct. An operator copies that file, so a key that drifted out
// of the struct while staying in the example is the #2553 shape with extra
// steps: documented, copied, ignored.
func TestExampleConfigHasNoUnknownKeys(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no config.example.yaml: %v", err)
	}
	if len(data) < 100 {
		t.Fatalf("config.example.yaml is %d bytes — a near-empty read would make this pass vacuously", len(data))
	}
	if got := findUnknownKeys(data); len(got) != 0 {
		for _, u := range got {
			t.Errorf("config.example.yaml line %d: key %q has no struct field", u.Line, u.Path)
		}
	}
}

// TestUnknownKeyDoesNotChangeLoadResult pins the "diagnostic only" contract: a
// config with an unknown key must decode to exactly the same Config as the same
// file without it. Reported, never acted on.
func TestUnknownKeyDoesNotChangeLoadResult(t *testing.T) {
	clean := "server:\n  dashboard_token: \"t\"\nsession:\n  cwd: /tmp\n"
	dirty := clean + "server_typo_block:\n  nope: 1\n"

	a, err := Load(writeCfg(t, clean))
	if err != nil {
		t.Fatalf("Load clean: %v", err)
	}
	b, err := Load(writeCfg(t, dirty))
	if err != nil {
		t.Fatalf("Load dirty: %v", err)
	}
	// Fingerprint differs by construction (different bytes and path); everything
	// else must match.
	a.Fingerprint = Fingerprint{}
	b.Fingerprint = Fingerprint{}
	if !reflect.DeepEqual(a, b) {
		t.Error("an unknown key changed the loaded Config; it must be diagnostic only")
	}
}

// TestUnknownKeyLineNumbersSurviveEnvExpansion pins why the line numbers can be
// reported at all: findUnknownKeys walks the ${VAR}-expanded bytes, not the
// operator's file, so it is only correct because expandEnvVars refuses any
// value containing a newline. If that guard is ever relaxed, the numbers in
// these diags start pointing at the wrong lines.
func TestUnknownKeyLineNumbersSurviveEnvExpansion(t *testing.T) {
	t.Setenv("NAOZHI_TEST_MULTILINE", "one\ntwo\nthree")
	body := "server:\n  dashboard_token: \"${NAOZHI_TEST_MULTILINE}\"\n  bogus_key: 1\n"
	got := findUnknownKeys(expandEnvVars([]byte(body)))
	if len(got) != 1 {
		t.Fatalf("got %+v, want exactly one unknown key", got)
	}
	if got[0].Line != 3 {
		t.Errorf("line = %d, want 3 — a multi-line expansion shifted the line numbers", got[0].Line)
	}
}

// TestUnknownKeyReportIsCapped keeps a garbage file from turning into thousands
// of Warn lines, and requires the overflow to be stated rather than silently
// truncated.
func TestUnknownKeyReportIsCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("server:\n")
	const n = maxUnknownKeyDiags + 7
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  bogus_%d: 1\n", i)
	}
	diags, _, err := collectLoadDiags(t, writeCfg(t, b.String()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := unknownDiags(diags)
	if len(got) != maxUnknownKeyDiags+1 {
		t.Fatalf("got %d diags, want %d listed + 1 overflow", len(got), maxUnknownKeyDiags)
	}
	last := got[len(got)-1]
	if last.Key != "(more)" || !strings.Contains(last.Reason, "7 further") {
		t.Errorf("overflow diag must state the remaining count, got %+v", last)
	}
}

// TestUnknownKeysNeverFatal covers the boot-safety promise: whatever the file
// contains, findUnknownKeys must not be the thing that stops naozhi starting.
// A stale key in a live config.yaml turning into "will not boot after upgrade"
// is worse than the silence this change removes.
func TestUnknownKeysNeverFatal(t *testing.T) {
	for _, body := range []string{
		"",
		"not_a_mapping\n",
		"- a\n- b\n",
		"anchor: &a\n  x: 1\nalias: *a\nbogus: 1\n",
		"server:\n  bogus: [1, 2, {deep: true}]\n",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("findUnknownKeys panicked on %q: %v", body, r)
				}
			}()
			findUnknownKeys([]byte(body))
		}()
	}
}
