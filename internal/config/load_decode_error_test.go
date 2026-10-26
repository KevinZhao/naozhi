package config

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// Shapes Load cannot easily produce: each must drop the file-derived text,
// which after ${VAR} expansion may be a secret.
func TestDescribeDecodeErrorDropsFileText(t *testing.T) {
	for _, err := range []error{
		&yaml.TypeError{Errors: []string{"line 5: cannot unmarshal !sk-TAG `v` into bool"}},
		&yaml.TypeError{Errors: []string{`line 9: mapping key "sk-KEY" already defined at line 7`}},
		&yaml.TypeError{Errors: []string{"line 6: something new `sk-NEW`"}},
		&yaml.TypeError{Errors: []string{"line 6: cannot unmarshal !!str `v` into sk-`BAD`"}},
		&yaml.TypeError{Errors: []string{"sk-NOLINE"}},
		errors.New("yaml: cannot decode !!str `sk-PLAIN` as a !!null"),
	} {
		if got := describeDecodeError(err); strings.Contains(got, "sk-") {
			t.Errorf("describeDecodeError(%q) = %q echoes file text", err, got)
		}
	}
}
