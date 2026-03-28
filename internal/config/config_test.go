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
		ttl      string
		expected time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"1h", time.Hour},
		{"", 30 * time.Minute},
		{"invalid", 30 * time.Minute},
	}

	for _, tt := range tests {
		cfg := &Config{Session: SessionConfig{TTL: tt.ttl}}
		got := cfg.ParseTTL()
		if got != tt.expected {
			t.Errorf("ParseTTL(%q) = %v, want %v", tt.ttl, got, tt.expected)
		}
	}
}

func TestParseWatchdog(t *testing.T) {
	tests := []struct {
		name           string
		noOutput       string
		total          string
		expectNoOutput time.Duration
		expectTotal    time.Duration
	}{
		{
			name:           "configured values",
			noOutput:       "120s",
			total:          "300s",
			expectNoOutput: 120 * time.Second,
			expectTotal:    300 * time.Second,
		},
		{
			name:           "defaults when empty",
			noOutput:       "",
			total:          "",
			expectNoOutput: 2 * time.Minute,
			expectTotal:    5 * time.Minute,
		},
		{
			name:           "defaults when invalid",
			noOutput:       "bad",
			total:          "bad",
			expectNoOutput: 2 * time.Minute,
			expectTotal:    5 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Session: SessionConfig{
					Watchdog: WatchdogConfig{
						NoOutputTimeout: tt.noOutput,
						TotalTimeout:    tt.total,
					},
				},
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
	os.WriteFile(tmpFile, []byte("{}"), 0644)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("default addr = %q, want %q", cfg.Server.Addr, ":8080")
	}
	if cfg.CLI.Model != "sonnet" {
		t.Errorf("default model = %q, want %q", cfg.CLI.Model, "sonnet")
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
    url: "http://10.0.0.2:8180"
    token: "secret"
    display_name: "MacBook Pro"
`), 0644)

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(cfg.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(cfg.Nodes))
	}
	n := cfg.Nodes["macbook"]
	if n.URL != "http://10.0.0.2:8180" {
		t.Errorf("url = %q", n.URL)
	}
	if n.Token != "secret" {
		t.Errorf("token = %q", n.Token)
	}
	if n.DisplayName != "MacBook Pro" {
		t.Errorf("display_name = %q", n.DisplayName)
	}
}

func TestLoadNodeConfig_TrailingSlash(t *testing.T) {
	tmpFile := t.TempDir() + "/config.yaml"
	os.WriteFile(tmpFile, []byte(`
nodes:
  bad:
    url: "http://10.0.0.2:8180/"
`), 0644)

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
`), 0644)

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
`), 0644)

	_, err := Load(tmpFile)
	if err == nil {
		t.Fatal("expected error for missing URL")
	}
}
