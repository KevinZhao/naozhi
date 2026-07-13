package config

import "testing"

func TestValidateAccessProfiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mut     func(c *Config)
		wantErr bool
	}{
		{
			name: "valid 1p profile",
			mut: func(c *Config) {
				c.AccessProfiles = map[string]AccessProfile{
					"1p": {
						Env:          map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"},
						DefaultModel: "claude-fable-5",
					},
				}
			},
		},
		{
			name: "valid bedrock profile via loopback proxy",
			mut: func(c *Config) {
				c.AccessProfiles = map[string]AccessProfile{
					"bedrock": {Env: map[string]string{
						"CLAUDE_CODE_USE_BEDROCK":    "1",
						"ANTHROPIC_BEDROCK_BASE_URL": "http://127.0.0.1:8889",
						"AWS_REGION":                 "us-west-2",
					}},
				}
			},
		},
		{
			name: "env key not in overlay allowlist",
			mut: func(c *Config) {
				c.AccessProfiles = map[string]AccessProfile{
					"bad": {Env: map[string]string{"AWS_PROFILE": "admin"}},
				}
			},
			wantErr: true,
		},
		{
			name: "env base url SSRF rejected",
			mut: func(c *Config) {
				c.AccessProfiles = map[string]AccessProfile{
					"bad": {Env: map[string]string{"ANTHROPIC_BASE_URL": "http://169.254.169.254"}},
				}
			},
			wantErr: true,
		},
		{
			name: "profile default_backend not enabled",
			mut: func(c *Config) {
				c.AccessProfiles = map[string]AccessProfile{
					"bad": {DefaultBackend: "nonexistent"},
				}
			},
			wantErr: true,
		},
		{
			name: "agent backend not enabled",
			mut: func(c *Config) {
				c.Agents = map[string]AgentConfig{"x": {Backend: "ghost"}}
			},
			wantErr: true,
		},
		{
			name: "agent access_profile undefined",
			mut: func(c *Config) {
				c.Agents = map[string]AgentConfig{"x": {AccessProfile: "missing"}}
			},
			wantErr: true,
		},
		{
			name: "agent access_profile defined ok",
			mut: func(c *Config) {
				c.AccessProfiles = map[string]AccessProfile{"p": {}}
				c.Agents = map[string]AgentConfig{"x": {AccessProfile: "p"}}
			},
		},
		{
			name: "default_access_profile defined ok",
			mut: func(c *Config) {
				c.AccessProfiles = map[string]AccessProfile{"bedrock": {}}
				c.DefaultAccessProfile = "bedrock"
			},
		},
		{
			name: "default_access_profile undefined",
			mut: func(c *Config) {
				c.AccessProfiles = map[string]AccessProfile{"1p": {}}
				c.DefaultAccessProfile = "bedrock"
			},
			wantErr: true,
		},
		{
			name: "default_access_profile empty ok (legacy fallthrough)",
			mut: func(c *Config) {
				c.AccessProfiles = map[string]AccessProfile{"1p": {}}
				c.DefaultAccessProfile = ""
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{}
			tc.mut(cfg)
			err := validateAccessProfiles(cfg)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateAccessProfiles() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
