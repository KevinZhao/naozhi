package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validateConfig was one 240-line function; it is now five per-section
// validators called in a fixed order. Each returns on its FIRST problem, so the
// order decides which of several bad values an operator is told about — and a
// refactor that reshuffles the calls silently changes the error an operator sees
// on a config with two mistakes. This pins the order by feeding configs that are
// wrong in two sections at once and asserting which complaint wins.
func TestValidateConfig_SectionOrderIsStable(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return p
	}

	cases := []struct {
		name string
		body string
		want string // substring of the error that must win
	}{
		{
			// platforms before nodes
			name: "platform beats node",
			body: `
platforms:
  slack:
    bot_token: ""
nodes:
  bad:
    url: ""
`,
			want: "slack",
		},
		{
			// nodes before server
			name: "node beats dashboard token",
			body: `
nodes:
  bad:
    url: "ftp://example.com"
server:
  dashboard_token: "${UNSET_VAR_FOR_TEST}"
`,
			want: "node",
		},
		{
			// server before notify
			name: "dashboard token beats notify platform",
			body: `
server:
  dashboard_token: "${UNSET_VAR_FOR_TEST}"
cron:
  notify_default:
    platform: "nosuchplatform"
`,
			want: "dashboard_token",
		},
		{
			// notify before argv-bearing fields
			name: "notify platform beats bad model",
			body: `
cron:
  notify_default:
    platform: "nosuchplatform"
cli:
  model: "bad model with spaces"
`,
			want: "notify",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.body))
			if err == nil {
				t.Fatal("Load must fail on a config that is wrong in two sections")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("first error = %q, want the %q section to win", err, tc.want)
			}
		})
	}
}

// The schema-version check runs before every section: a config from a newer
// binary must be refused as such, not misreported as whichever section happens
// to parse badly under this binary's schema.
func TestValidateConfig_SchemaVersionWinsOverEverything(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := `
schema_version: 999
platforms:
  slack:
    bot_token: ""
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("a newer schema_version must be refused")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("error = %q, want the schema_version refusal to win", err)
	}
}
