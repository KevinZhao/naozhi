package shim

import (
	"strings"
	"testing"
)

// TestFilterShimEnv_ProfileValidation pins [R20260603-SEC-1]: AWS_PROFILE /
// AWS_DEFAULT_PROFILE values must match ^[A-Za-z0-9_-]{1,64}$ before being
// forwarded to the CLI subprocess. A profile name carrying shell
// metacharacters or path traversal could redirect the AWS SDK's
// credential_process lookup to an attacker-controlled profile.
func TestFilterShimEnv_ProfileValidation(t *testing.T) {
	t.Parallel()

	// Only AWS_PROFILE is in shimEnvAllowedPrefixes (AWS_DEFAULT_PROFILE is
	// intentionally not forwarded), so the filterShimEnv integration cases use
	// AWS_PROFILE. The validator's coverage of AWS_DEFAULT_PROFILE is asserted
	// separately via shimProfileEnvDropped below.
	const profileKey = "AWS_PROFILE="

	cases := []struct {
		name     string
		entry    string
		wantKept bool
	}{
		{"legal_simple", profileKey + "prod", true},
		{"legal_underscore_hyphen", profileKey + "my_prod-profile_1", true},
		{"legal_max_len", profileKey + strings.Repeat("a", 64), true},
		{"reject_semicolon", profileKey + "prod;rm -rf /", false},
		{"reject_path_traversal", profileKey + "../../evil", false},
		{"reject_slash", profileKey + "a/b", false},
		{"reject_space", profileKey + "prod profile", false},
		{"reject_empty", profileKey + "", false},
		{"reject_too_long", profileKey + strings.Repeat("a", 65), false},
		{"reject_metachar", profileKey + "$(whoami)", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := filterShimEnv([]string{tc.entry})
			kept := len(got) == 1 && got[0] == tc.entry
			if kept != tc.wantKept {
				t.Fatalf("filterShimEnv(%q) kept=%v, want %v (got=%v)", tc.entry, kept, tc.wantKept, got)
			}
		})
	}
}

// TestShimProfileEnvDropped covers the validator directly, including
// AWS_DEFAULT_PROFILE (which is not in the forward allowlist but the validator
// must still reject unsafe values defensively). [R20260603-SEC-1]
func TestShimProfileEnvDropped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		entry       string
		wantDropped bool
	}{
		{"AWS_PROFILE=prod", false},
		{"AWS_DEFAULT_PROFILE=staging-2", false},
		{"AWS_PROFILE=../../evil", true},
		{"AWS_DEFAULT_PROFILE=$(whoami)", true},
		{"AWS_DEFAULT_PROFILE=a;b", true},
		// Non-profile keys must never be treated as profiles.
		{"AWS_REGION=us-east-1", false},
		{"HOME=/home/user", false},
	}
	for _, tc := range cases {
		if got := shimProfileEnvDropped(tc.entry); got != tc.wantDropped {
			t.Errorf("shimProfileEnvDropped(%q) = %v, want %v", tc.entry, got, tc.wantDropped)
		}
	}
}
