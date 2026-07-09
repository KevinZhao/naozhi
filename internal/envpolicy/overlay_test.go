package envpolicy

import "testing"

func TestValidateOverlayEntry(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{"bedrock selector on", "CLAUDE_CODE_USE_BEDROCK", "1", false},
		{"bedrock selector off", "CLAUDE_CODE_USE_BEDROCK", "0", false},
		{"skip bedrock auth", "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "1", false},
		{"anthropic base url https", "ANTHROPIC_BASE_URL", "https://api.anthropic.com", false},
		{"bedrock base url loopback http", "ANTHROPIC_BEDROCK_BASE_URL", "http://127.0.0.1:8889", false},
		{"model pin", "ANTHROPIC_MODEL", "claude-opus-4-8", false},
		{"region ok", "AWS_REGION", "us-west-2", false},
		{"default region ok", "AWS_DEFAULT_REGION", "us-east-1", false},
		{"token file abs", "ANTHROPIC_AUTH_TOKEN_FILE", "/home/ec2-user/.secrets/t.token", false},
		{"api key file abs", "ANTHROPIC_API_KEY_FILE", "/etc/naozhi/key", false},

		// rejections
		{"empty key", "", "x", true},
		{"aws profile not overlay-settable", "AWS_PROFILE", "admin", true},
		{"aws creds not overlay-settable", "AWS_ACCESS_KEY_ID", "AKIA...", true},
		{"aws creds file not overlay-settable", "AWS_SHARED_CREDENTIALS_FILE", "/x", true},
		{"arbitrary key", "PATH", "/usr/bin", true},
		{"claude kill switch", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1", true},
		{"base url plain http non-loopback", "ANTHROPIC_BASE_URL", "http://evil.example.com", true},
		{"base url IMDS", "ANTHROPIC_BASE_URL", "http://169.254.169.254", true},
		{"bedrock base url IMDS https", "ANTHROPIC_BEDROCK_BASE_URL", "https://169.254.169.254", true},
		{"region with metachar", "AWS_REGION", "us-west-2; rm -rf /", true},
		{"token file relative", "ANTHROPIC_AUTH_TOKEN_FILE", "secrets/t.token", true},
		{"token file traversal", "ANTHROPIC_AUTH_TOKEN_FILE", "/home/../etc/shadow", true},
		{"token file null byte", "ANTHROPIC_AUTH_TOKEN_FILE", "/home/t\x00", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOverlayEntry(tt.key, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateOverlayEntry(%q, %q) err = %v, wantErr = %v", tt.key, tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestResolvedFileKey(t *testing.T) {
	if k, ok := ResolvedFileKey("ANTHROPIC_AUTH_TOKEN_FILE"); !ok || k != "ANTHROPIC_AUTH_TOKEN" {
		t.Errorf("ResolvedFileKey(auth token file) = %q, %v", k, ok)
	}
	if k, ok := ResolvedFileKey("ANTHROPIC_API_KEY_FILE"); !ok || k != "ANTHROPIC_API_KEY" {
		t.Errorf("ResolvedFileKey(api key file) = %q, %v", k, ok)
	}
	if _, ok := ResolvedFileKey("ANTHROPIC_BASE_URL"); ok {
		t.Errorf("ResolvedFileKey(non-file key) should be false")
	}
}
