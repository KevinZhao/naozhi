package config

import (
	"strings"
	"testing"
)

// TestLoadDecodeErrorDoesNotEchoSecret is the reason the description is built
// from parts: yaml.v3 quotes a value's first 7 bytes, or all of a value of 10
// bytes or fewer, and after ${VAR} expansion that value can be a secret.
func TestLoadDecodeErrorDoesNotEchoSecret(t *testing.T) {
	cases := []struct {
		name, secret string
		forbidden    []string
	}{
		{name: "long value", secret: "sk-SECRET-VALUE-1234", forbidden: []string{"sk-SECRET-VALUE-1234", "sk-SECR"}},
		{name: "short value", secret: "sk-SHORT1", forbidden: []string{"sk-SHORT1", "sk-SHO"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NAOZHI_TEST_DECODE_SECRET", tc.secret)
			for _, field := range []string{"cron:\n  auto_pause_after_failures", "server:\n  debug_mode"} {
				_, err := Load(writeCfg(t, field+": ${NAOZHI_TEST_DECODE_SECRET}\n"))
				if err == nil {
					t.Fatalf("%s: Load succeeded, want a decode error", field)
				}
				if !strings.Contains(err.Error(), "line 2: cannot unmarshal !!str into") {
					t.Errorf("err = %q, want the line and target type", err)
				}
				for _, f := range tc.forbidden {
					if strings.Contains(err.Error(), f) {
						t.Errorf("err = %q echoes %q", err, f)
					}
				}
			}
		})
	}
}
