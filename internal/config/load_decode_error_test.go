package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestLoadDecodeErrorIsNotSyntaxError pins the operator-facing text of a
// Decode failure: the file parsed, so it must not be called a syntax error,
// and it must carry the line and the target type.
func TestLoadDecodeErrorIsNotSyntaxError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "string into int",
			body: "cron:\n  auto_pause_after_failures: abc\n",
			want: []string{"yaml type error", "line 2: cannot unmarshal !!str into int"},
		},
		{
			name: "string into bool",
			body: "server:\n  debug_mode: yes please\n",
			want: []string{"yaml type error", "line 2: cannot unmarshal !!str into bool"},
		},
		{
			name: "sequence into struct",
			body: "cron: [1, 2]\n",
			want: []string{"line 1: cannot unmarshal !!seq into config.CronConfig"},
		},
		{
			name: "duplicate key",
			body: "cron:\n  auto_pause_after_failures: 1\n  auto_pause_after_failures: 2\n",
			want: []string{"line 3: duplicate key (first defined at line 2)"},
		},
		{
			name: "non-TypeError decode failure",
			body: "cron:\n  jitter_max: !!binary \"@@@\"\n",
			want: []string{"parse config: yaml decode error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil {
				t.Fatal("Load succeeded, want a decode error")
			}
			msg := err.Error()
			if strings.Contains(msg, "syntax") {
				t.Errorf("err = %q, a decode failure must not be reported as a syntax error", msg)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("err = %q, want it to contain %q", msg, w)
				}
			}
			if strings.Contains(msg, "auto_pause_after_failures") {
				t.Errorf("err = %q echoes a key from the file", msg)
			}
		})
	}
}

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

// TestLoadSyntaxErrorWordingUnchanged: a file that does not parse at all is
// still a syntax error.
func TestLoadSyntaxErrorWordingUnchanged(t *testing.T) {
	_, err := Load(writeCfg(t, "cron: : :\n"))
	if err == nil || !strings.Contains(err.Error(), "parse config: yaml syntax error") {
		t.Fatalf("err = %v, want a yaml syntax error", err)
	}
}

func TestDescribeDecodeError(t *testing.T) {
	many := make([]string, 8)
	for i := range many {
		many[i] = fmt.Sprintf("line %d: cannot unmarshal !!str `x` into int", i+1)
	}
	cases := []struct {
		name   string
		err    error
		want   string
		absent []string
	}{
		{
			name: "standard tag and value dropped",
			err:  &yaml.TypeError{Errors: []string{"line 4: cannot unmarshal !!str `hunter2` into int"}},
			want: "yaml type error: line 4: cannot unmarshal !!str into int",
		},
		{
			name: "map type with a space",
			err:  &yaml.TypeError{Errors: []string{"line 2: cannot unmarshal !!seq into map[string]interface {}"}},
			want: "yaml type error: line 2: cannot unmarshal !!seq into map[string]interface {}",
		},
		{
			name: "value containing into cannot move the split",
			err:  &yaml.TypeError{Errors: []string{"line 3: cannot unmarshal !!str `a into b` into int"}},
			want: "yaml type error: line 3: cannot unmarshal !!str into int",
		},
		{
			name:   "custom tag dropped",
			err:    &yaml.TypeError{Errors: []string{"line 5: cannot unmarshal !sk-TAG `v` into bool"}},
			want:   "yaml type error: line 5: cannot unmarshal into bool",
			absent: []string{"sk-TAG"},
		},
		{
			name:   "duplicate key text dropped",
			err:    &yaml.TypeError{Errors: []string{`line 9: mapping key "sk-KEY" already defined at line 7`}},
			want:   "yaml type error: line 9: duplicate key (first defined at line 7)",
			absent: []string{"sk-KEY"},
		},
		{
			name:   "unknown shape keeps only the line",
			err:    &yaml.TypeError{Errors: []string{"line 6: something new `sk-NEW`"}},
			want:   "yaml type error: line 6: invalid value",
			absent: []string{"sk-NEW"},
		},
		{
			name:   "target type that is not a type name",
			err:    &yaml.TypeError{Errors: []string{"line 6: cannot unmarshal !!str `v` into sk-`BAD`"}},
			want:   "yaml type error: line 6: invalid value",
			absent: []string{"sk-"},
		},
		{
			name:   "no line number",
			err:    &yaml.TypeError{Errors: []string{"sk-NOLINE"}},
			want:   "yaml type error: invalid value",
			absent: []string{"sk-NOLINE"},
		},
		{
			name:   "non-TypeError",
			err:    errors.New("yaml: cannot decode !!str `sk-PLAIN` as a !!null"),
			want:   "yaml decode error",
			absent: []string{"sk-PLAIN"},
		},
		{
			name:   "wrapped TypeError",
			err:    fmt.Errorf("wrap: %w", &yaml.TypeError{Errors: []string{"line 1: cannot unmarshal !!map into string"}}),
			want:   "yaml type error: line 1: cannot unmarshal !!map into string",
			absent: []string{"wrap"},
		},
		{
			name: "entries capped at five",
			err:  &yaml.TypeError{Errors: many},
			want: "yaml type error: line 1: cannot unmarshal !!str into int; line 2: cannot unmarshal !!str into int; " +
				"line 3: cannot unmarshal !!str into int; line 4: cannot unmarshal !!str into int; " +
				"line 5: cannot unmarshal !!str into int (+3 more)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := describeDecodeError(tc.err)
			if got != tc.want {
				t.Errorf("describeDecodeError = %q, want %q", got, tc.want)
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("describeDecodeError = %q echoes %q", got, a)
				}
			}
		})
	}
}
