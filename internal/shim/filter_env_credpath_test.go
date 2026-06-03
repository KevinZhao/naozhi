package shim

import (
	"strings"
	"testing"
)

// TestFilterShimEnv_CredPathValidation pins [R20260603-SEC-2]:
// AWS_SHARED_CREDENTIALS_FILE / AWS_CONFIG_FILE / AWS_WEB_IDENTITY_TOKEN_FILE
// values must be absolute, traversal-free, null-free paths before being
// forwarded to the CLI subprocess. A tampered value pointing at
// /proc/self/environ, /etc/shadow, or a relative traversal could make the AWS
// SDK read an arbitrary host file and treat it as a credential / OIDC token.
func TestFilterShimEnv_CredPathValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		entry    string
		wantKept bool
	}{
		{"legal_abs_credentials", "AWS_SHARED_CREDENTIALS_FILE=/home/user/.aws/credentials", true},
		{"legal_abs_config", "AWS_CONFIG_FILE=/etc/aws/config", true},
		{"legal_abs_token", "AWS_WEB_IDENTITY_TOKEN_FILE=/var/run/secrets/token", true},
		{"legal_mounted_root", "AWS_SHARED_CREDENTIALS_FILE=/mnt/secrets/creds", true},
		{"reject_relative", "AWS_SHARED_CREDENTIALS_FILE=.aws/credentials", false},
		{"reject_traversal", "AWS_SHARED_CREDENTIALS_FILE=/home/user/../../etc/shadow", false},
		{"reject_traversal_leading", "AWS_CONFIG_FILE=../../etc/shadow", false},
		{"reject_proc_relative", "AWS_WEB_IDENTITY_TOKEN_FILE=proc/self/environ", false},
		{"reject_empty", "AWS_SHARED_CREDENTIALS_FILE=", false},
		// Note: a plain absolute /proc or /etc path is structurally valid; the
		// guard blocks traversal/relative escape, which is the injection vector
		// a poisoned rc would use to point outside an intended root.
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

// TestShimCredPathEnvDropped covers the validator directly. [R20260603-SEC-2]
func TestShimCredPathEnvDropped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		entry       string
		wantDropped bool
	}{
		{"AWS_SHARED_CREDENTIALS_FILE=/home/u/.aws/credentials", false},
		{"AWS_CONFIG_FILE=/etc/aws/config", false},
		{"AWS_WEB_IDENTITY_TOKEN_FILE=/var/run/token", false},
		{"AWS_SHARED_CREDENTIALS_FILE=../escape", true},
		{"AWS_CONFIG_FILE=/a/../../b", true},
		{"AWS_WEB_IDENTITY_TOKEN_FILE=relative/path", true},
		{"AWS_SHARED_CREDENTIALS_FILE=", true},
		{"AWS_SHARED_CREDENTIALS_FILE=/has\x00null", true},
		// Non-path keys must never be treated as credential paths.
		{"AWS_REGION=us-east-1", false},
		{"AWS_PROFILE=prod", false},
		{"HOME=/home/user", false},
	}
	for _, tc := range cases {
		if got := shimCredPathEnvDropped(tc.entry); got != tc.wantDropped {
			t.Errorf("shimCredPathEnvDropped(%q) = %v, want %v", tc.entry, got, tc.wantDropped)
		}
	}
}

// TestIsSafeShimCredPath covers the path predicate directly. [R20260603-SEC-2]
func TestIsSafeShimCredPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		val  string
		want bool
	}{
		{"/abs/path", true},
		{"/a/b/c", true},
		{"", false},
		{"relative", false},
		{"./relative", false},
		{"/a/../b", false},
		{"../a", false},
		{"/a/b/..", false},
		{"/has\x00null", false},
		{strings.Repeat("/a", 100), true},
	}
	for _, tc := range cases {
		if got := isSafeShimCredPath(tc.val); got != tc.want {
			t.Errorf("isSafeShimCredPath(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}
