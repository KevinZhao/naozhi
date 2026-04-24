package config

import (
	"os"
	"testing"
	"time"
)

func TestExpandEnvVars(t *testing.T) {
	os.Setenv("TEST_NAOZHI_VAR", "hello")
	defer os.Unsetenv("TEST_NAOZHI_VAR")

	tests := []struct {
		input    string
		expected string
	}{
		{"${TEST_NAOZHI_VAR}", "hello"},
		{"prefix-${TEST_NAOZHI_VAR}-suffix", "prefix-hello-suffix"},
		{"${UNSET_VAR_12345}", "${UNSET_VAR_12345}"},
		{"no vars here", "no vars here"},
		{"${TEST_NAOZHI_VAR} and ${TEST_NAOZHI_VAR}", "hello and hello"},
		{"", ""},
	}

	for _, tt := range tests {
		got := expandEnvVars(tt.input)
		if got != tt.expected {
			t.Errorf("expandEnvVars(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestParseTTL(t *testing.T) {
	tests := []struct {
		yaml     string
		expected time.Duration
		wantErr  bool
	}{
		{`session: {ttl: "30m"}`, 30 * time.Minute, false},
		{`session: {ttl: "1h"}`, time.Hour, false},
		{`{}`, 30 * time.Minute, false},    // empty → default
		{`session: {ttl: "bad"}`, 0, true}, // invalid → error
		{`session: {ttl: "-5m"}`, 0, true}, // non-positive → error
	}

	for _, tt := range tests {
		tmpFile := t.TempDir() + "/config.yaml"
		os.WriteFile(tmpFile, []byte(tt.yaml), 0600)
		cfg, err := Load(tmpFile)
		if tt.wantErr {
			if err == nil {
				t.Errorf("Load(%q) expected error, got nil", tt.yaml)
			}
			continue
		}
		if err != nil {
			t.Errorf("Load(%q) unexpected error: %v", tt.yaml, err)
			continue
		}
		got := cfg.ParseTTL()
		if got != tt.expected {
			t.Errorf("ParseTTL() = %v, want %v (yaml: %q)", got, tt.expected, tt.yaml)
		}
	}
}

func TestParseWatchdog(t *testing.T) {
	tests := []struct {
		name           string
		yaml           string
		expectNoOutput time.Duration
		expectTotal    time.Duration
		wantErr        bool
	}{
		{
			name:           "configured values",
			yaml:           `session: {watchdog: {no_output_timeout: "120s", total_timeout: "300s"}}`,
			expectNoOutput: 120 * time.Second,
			expectTotal:    300 * time.Second,
		},
		{
			name:           "defaults when empty",
			yaml:           `{}`,
			expectNoOutput: 2 * time.Minute,
			expectTotal:    5 * time.Minute,
		},
		{
			name:    "error on invalid no_output_timeout",
			yaml:    `session: {watchdog: {no_output_timeout: "bad"}}`,
			wantErr: true,
		},
		{
			name:    "error on invalid total_timeout",
			yaml:    `session: {watchdog: {total_timeout: "bad"}}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := t.TempDir() + "/config.yaml"
			os.WriteFile(tmpFile, []byte(tt.yaml), 0600)
			cfg, err := Load(tmpFile)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Load(%q) expected error, got nil", tt.yaml)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			gotNoOutput, gotTotal := cfg.ParseWatchdog()
			if gotNoOutput != tt.expectNoOutput {
				t.Errorf("NoOutputTimeout = %v, want %v", gotNoOutput, tt.expectNoOutput)
			}
			if gotTotal != tt.expectTotal {
				t.Errorf("TotalTimeout = %v, want %v", gotTotal, tt.expectTotal)
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte("{}"), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("default addr = %q, want %q", cfg.Server.Addr, ":8080")
	}
	if cfg.CLI.Model != "" {
		t.Errorf("default model = %q, want empty", cfg.CLI.Model)
	}
	if cfg.Session.MaxProcs != 3 {
		t.Errorf("default max_procs = %d, want 3", cfg.Session.MaxProcs)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("default log level = %q, want %q", cfg.Log.Level, "info")
	}
}

func TestLoadNodeConfig(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
nodes:
  macbook:
    url: "https://10.0.0.2:8180"
    token: "secret"
    display_name: "MacBook Pro"
`), 0600)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(cfg.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(cfg.Nodes))
	}
	n := cfg.Nodes["macbook"]
	if n.URL != "https://10.0.0.2:8180" {
		t.Errorf("url = %q", n.URL)
	}
	if n.Token != "secret" {
		t.Errorf("token = %q", n.Token)
	}
	if n.DisplayName != "MacBook Pro" {
		t.Errorf("display_name = %q", n.DisplayName)
	}
}

func TestLoadNodeConfig_HTTPWithToken(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
nodes:
  bad:
    url: "http://10.0.0.2:8180"
    token: "secret"
`), 0600)

	_, err := Load(tmpFile)
	if err == nil {
		t.Fatal("expected error for HTTP with bearer token")
	}
}

func TestLoadNodeConfig_TrailingSlash(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
nodes:
  bad:
    url: "http://10.0.0.2:8180/"
`), 0600)

	_, err := Load(tmpFile)
	if err == nil {
		t.Fatal("expected error for trailing slash")
	}
}

func TestLoadNodeConfig_InvalidScheme(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
nodes:
  bad:
    url: "ftp://10.0.0.2:8180"
`), 0600)

	_, err := Load(tmpFile)
	if err == nil {
		t.Fatal("expected error for non-http URL")
	}
}

func TestLoadNodeConfig_MissingURL(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
nodes:
  bad:
    token: "secret"
`), 0600)

	_, err := Load(tmpFile)
	if err == nil {
		t.Fatal("expected error for missing URL")
	}
}

func TestEnabledBackends(t *testing.T) {
	tests := []struct {
		name           string
		cfg            Config
		wantIDs        []string // expected IDs in order
		wantDefaultID  string
		wantFirstModel string // model on out[0]
	}{
		{
			name: "legacy single backend defaults to claude",
			cfg: Config{CLI: CLIConfig{
				Model: "sonnet",
			}},
			wantIDs:        []string{"claude"},
			wantDefaultID:  "claude",
			wantFirstModel: "sonnet",
		},
		{
			name: "legacy single backend kiro",
			cfg: Config{CLI: CLIConfig{
				Backend: "kiro",
				Model:   "sonnet",
			}},
			wantIDs:        []string{"kiro"},
			wantDefaultID:  "kiro",
			wantFirstModel: "sonnet",
		},
		{
			name: "multi-backend floats default first",
			cfg: Config{CLI: CLIConfig{
				Backend: "kiro",
				Model:   "sonnet",
				Backends: []CLIBackendConfig{
					{ID: "claude"},
					{ID: "kiro", Model: "gpt-5"},
				},
			}},
			wantIDs:        []string{"kiro", "claude"},
			wantDefaultID:  "kiro",
			wantFirstModel: "gpt-5", // per-backend model wins
		},
		{
			name: "multi-backend falls back to global model when per-backend empty",
			cfg: Config{CLI: CLIConfig{
				Model: "sonnet",
				Backends: []CLIBackendConfig{
					{ID: "claude"},
					{ID: "kiro"},
				},
			}},
			// Default "claude" (empty cli.backend defaults to "claude"),
			// already first in list.
			wantIDs:        []string{"claude", "kiro"},
			wantDefaultID:  "claude",
			wantFirstModel: "sonnet",
		},
		{
			name: "duplicate IDs collapse",
			cfg: Config{CLI: CLIConfig{
				Backends: []CLIBackendConfig{
					{ID: "claude"},
					{ID: "claude", Model: "opus"}, // duplicate dropped
					{ID: "kiro"},
				},
			}},
			wantIDs:       []string{"claude", "kiro"},
			wantDefaultID: "claude",
		},
		{
			// Regression guard for R54-F4: when cli.backend is unset and
			// the first cli.backends entry is not "claude", both
			// EnabledBackends()[0].ID and DefaultBackendID() must agree.
			name: "empty cli.backend picks first backend entry as default",
			cfg: Config{CLI: CLIConfig{
				Backends: []CLIBackendConfig{
					{ID: "kiro"},
					{ID: "claude"},
				},
			}},
			wantIDs:       []string{"kiro", "claude"},
			wantDefaultID: "kiro",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.EnabledBackends()
			ids := make([]string, len(got))
			for i, b := range got {
				ids[i] = b.ID
			}
			if len(ids) != len(tt.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tt.wantIDs)
			}
			for i, id := range ids {
				if id != tt.wantIDs[i] {
					t.Errorf("ids[%d] = %q, want %q", i, id, tt.wantIDs[i])
				}
			}
			if got := tt.cfg.DefaultBackendID(); got != tt.wantDefaultID {
				t.Errorf("DefaultBackendID = %q, want %q", got, tt.wantDefaultID)
			}
			if tt.wantFirstModel != "" && got[0].Model != tt.wantFirstModel {
				t.Errorf("out[0].Model = %q, want %q", got[0].Model, tt.wantFirstModel)
			}
			// R54-F4 contract: out[0].ID must equal DefaultBackendID(),
			// otherwise the router default diverges from the UI primary.
			if gotDef := tt.cfg.DefaultBackendID(); got[0].ID != gotDef {
				t.Errorf("invariant violated: out[0].ID = %q, DefaultBackendID = %q", got[0].ID, gotDef)
			}
		})
	}
}

// TestEnabledBackends_AllEmptyIDsFallback covers the operator-error case
// where cli.backends is set but every entry omits `id:`. Previously the
// dedup loop would silently drop every entry and return an empty slice,
// causing main.go to crash with "no usable cli backend configured".
// R54-F10: fall back to legacy single-backend mode instead.
func TestEnabledBackends_AllEmptyIDsFallback(t *testing.T) {
	cfg := Config{CLI: CLIConfig{
		Path:  "/usr/local/bin/claude",
		Model: "sonnet",
		Backends: []CLIBackendConfig{
			{Path: "/usr/local/bin/claude"}, // ID omitted
		},
	}}
	got := cfg.EnabledBackends()
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 fallback entry", len(got))
	}
	if got[0].ID != "claude" {
		t.Errorf("got[0].ID = %q, want fallback to default %q", got[0].ID, "claude")
	}
	if got[0].Path != "/usr/local/bin/claude" {
		t.Errorf("got[0].Path = %q, want cli.path passthrough", got[0].Path)
	}
}
