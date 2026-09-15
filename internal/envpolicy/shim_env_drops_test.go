package envpolicy

import (
	"strings"
	"testing"
)

// The shim env gate used to log its rejections and nothing else, so an operator
// whose AWS_PROFILE was refused had one log line and no counter, no config-check
// entry, no dashboard signal. ShimEnvDrops is the reportable half of the same
// verdict FilterShimEnv keeps; these tests pin that the two halves agree, that
// the security-relevant reasons survive, and that no value is ever echoed.

func TestShimEnvDrops_AgreesWithFilterShimEnv(t *testing.T) {
	t.Parallel()
	input := []string{
		"HOME=/home/user",                           // allowed, plain
		"AWS_PROFILE=prod",                          // allowed, safe profile
		"AWS_PROFILE=../../etc/passwd",              // gate: unsafe profile value
		"AWS_SHARED_CREDENTIALS_FILE=relative/x",    // gate: non-absolute cred path
		"ANTHROPIC_BASE_URL=http://169.254.169.254", // gate: IMDS over plain http
		"SOME_RANDOM_KEY=whatever",                  // outside the shim column: silent
	}
	kept := FilterShimEnv(input)
	drops := ShimEnvDrops(input)

	keptSet := map[string]bool{}
	for _, kv := range kept {
		keptSet[kv] = true
	}
	// Every gated entry is dropped from the env AND reported.
	for _, kv := range []string{
		"AWS_PROFILE=../../etc/passwd",
		"AWS_SHARED_CREDENTIALS_FILE=relative/x",
		"ANTHROPIC_BASE_URL=http://169.254.169.254",
	} {
		if keptSet[kv] {
			t.Errorf("%q reached the child env", kv)
		}
		key := kv[:strings.IndexByte(kv, '=')]
		if reportFor(drops, key).Key == "" {
			t.Errorf("%q was dropped with no report — that is the blind spot this replaces", kv)
		}
	}
	// A key the shim column simply does not carry is everyday noise, not a
	// rejection of configured input: reporting it would bury the real ones.
	if d := reportFor(drops, "SOME_RANDOM_KEY"); d.Key != "" {
		t.Errorf("key outside the shim column must not be reported, got %+v", d)
	}
	if !keptSet["HOME=/home/user"] || !keptSet["AWS_PROFILE=prod"] {
		t.Errorf("safe entries must survive, kept=%v", kept)
	}
}

// The reasons are the operator's only explanation of a refused credential var,
// so each guard's reason has to name its own hazard rather than a generic
// "dropped".
func TestShimEnvDrops_ReasonsNameTheGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		entry      string
		key        string
		wantInText string
	}{
		{"AWS_PROFILE=has space", "AWS_PROFILE", "credential_process"},
		{"AWS_CONFIG_FILE=/a/../../b", "AWS_CONFIG_FILE", "traversal"},
		{"ANTHROPIC_BASE_URL=http://evil.test", "ANTHROPIC_BASE_URL", "base_url"},
	}
	for _, tc := range cases {
		drops := ShimEnvDrops([]string{tc.entry})
		d := reportFor(drops, tc.key)
		if d.Key == "" {
			t.Errorf("%q produced no drop", tc.entry)
			continue
		}
		if !strings.Contains(d.Reason, tc.wantInText) {
			t.Errorf("reason for %q = %q, want it to mention %q", tc.entry, d.Reason, tc.wantInText)
		}
	}
}

// A value may be a credential; a report that echoed it would move the secret
// into the log the report exists to write.
func TestShimEnvDrops_NeverEchoesTheValue(t *testing.T) {
	t.Parallel()
	const secret = "AKIAIOSFODNN7EXAMPLE-secret-value"
	for _, entry := range []string{
		"AWS_PROFILE=" + secret + " with space",
		"AWS_SHARED_CREDENTIALS_FILE=relative/" + secret,
		"ANTHROPIC_BASE_URL=http://evil.test/" + secret,
	} {
		for _, d := range ShimEnvDrops([]string{entry}) {
			if strings.Contains(d.Reason, secret) || strings.Contains(d.Key, secret) {
				t.Errorf("%q leaked the value into its report: %+v", entry, d)
			}
		}
	}
}

// reportFor returns the drop for key, or the zero value.
func reportFor(drops []Drop, key string) Drop {
	for _, d := range drops {
		if d.Key == key {
			return d
		}
	}
	return Drop{}
}
