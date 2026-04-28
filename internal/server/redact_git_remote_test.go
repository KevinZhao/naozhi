package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestRedactGitRemoteURL_StripsUserinfo covers the happy-path contracts
// redactGitRemoteURL has been shipping since the Round 46 PAT-leak fix.
// Pin them as a table so future refactors (e.g. switching url.Parse for a
// custom scanner to handle broken URLs) cannot silently regress.
func TestRedactGitRemoteURL_StripsUserinfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty passthrough", "", ""},
		{"plain https no creds", "https://github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"https with PAT", "https://user:ghp_abcdef@github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"https with user only", "https://user@github.com/org/repo.git", "https://github.com/org/repo.git"},
		// SCP-style SSH URLs have no `://` → url.Parse reports Scheme="" and
		// we return raw unchanged. This is the common case git stores in
		// .git/config for ssh-cloned repos; the `git@` username is not
		// credential material.
		{"ssh scp form passthrough (no scheme)", "git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		// Full-scheme ssh:// URLs are rare in .git/config but technically
		// valid. The current redactor strips ALL userinfo including the
		// harmless `git@` because there is no reliable way to distinguish
		// a username from a username-as-credential token without parsing
		// ssh_config. Accept the tradeoff: on the remote URL page the
		// dashboard will show `ssh://github.com/...`, which is still a
		// valid clone URL because ssh defaults the user to `git` for
		// github.com. The alternative (leave u.User set) would let
		// `ssh://user:pat@...` leak the PAT.
		{"ssh url form userinfo stripped", "ssh://git@github.com/org/repo.git", "ssh://github.com/org/repo.git"},
		{"ssh url with password fully redacted", "ssh://git:secret@github.com/org/repo.git", "ssh://github.com/org/repo.git"},
		{"file scheme passthrough", "file:///home/u/repo", "file:///home/u/repo"},
		{"unparseable stays as-is", "not a url", "not a url"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactGitRemoteURL(tc.in)
			if got != tc.want {
				t.Errorf("redactGitRemoteURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Any output that still contains "ghp_", "glpat-", or "Bearer " is
			// a credential leak regardless of the happy-path expectation.
			for _, marker := range []string{"ghp_", "glpat-", "Bearer "} {
				if strings.Contains(got, marker) {
					t.Errorf("redactGitRemoteURL(%q) = %q contains credential marker %q",
						tc.in, got, marker)
				}
			}
		})
	}
}

// TestRedactGitRemoteURL_NodeCacheForwardIsRedacted is the R47-DESIGN-REMOTE-
// NODE-GIT-CREDENTIAL pin: every field we lift out of the node cache (which
// comes from a remote peer that may be running an older binary without its
// own redaction pass) MUST be passed through redactGitRemoteURL before
// landing in a dashboard JSON response.
//
// Today only `git_remote_url` is transferred — the source-level test below
// reads dashboard_session.go and asserts two invariants:
//
//  1. The merge loop still calls redactGitRemoteURL on the `git_remote_url`
//     field read from `item`.
//  2. No other "known credential-bearing fields" have been added to the
//     merge loop without a redactor. The current risk list: anything
//     matching `token|secret|password|credential|pat|auth_url|clone_url`
//     in the key name.
//
// If a future change pipes another field through (e.g. `deploy_key`,
// `clone_url_with_token`, a per-project proxy `credential` blob), the
// author must either redact it explicitly or extend this test's whitelist
// — either path forces the change to be reviewed through this audit item
// instead of silently leaking PATs.
func TestRedactGitRemoteURL_NodeCacheForwardIsRedacted(t *testing.T) {
	src, err := os.ReadFile("dashboard_session.go")
	if err != nil {
		t.Fatalf("read dashboard_session.go: %v", err)
	}

	// 1) redactGitRemoteURL must still wrap the node-cache git_remote_url
	// extraction. If someone "simplifies" it to a direct assignment, the
	// remote-peer-behind-on-patches scenario re-opens the PAT leak.
	redactedRead := regexp.MustCompile(
		`item\["git_remote_url"\]\.\(string\)[\s\S]{0,120}redactGitRemoteURL\(`)
	if !redactedRead.Match(src) {
		t.Error("dashboard_session.go no longer runs redactGitRemoteURL on the " +
			"node-cache forwarded `git_remote_url` field. R47-DESIGN-REMOTE-NODE-" +
			"GIT-CREDENTIAL: remote nodes may be older binaries that have not " +
			"redacted their own outputs; the primary MUST always scrub credentials " +
			"before surfacing node-cache data to the dashboard.")
	}

	// 2) No bare `item["<credential-key>"]` read without a redactor. We grep
	// for the known risk markers. `git_remote_url` is allowed (checked in
	// #1 above); any new match needs either its own redactor or an explicit
	// whitelist addition here.
	riskyKeyPattern := regexp.MustCompile(
		`item\["(?P<k>[a-z_]*(?:token|secret|password|credential|pat|auth_url|clone_url|deploy_key)[a-z_]*)"\]`)
	matches := riskyKeyPattern.FindAllStringSubmatch(string(src), -1)
	for _, m := range matches {
		key := m[1]
		if key == "git_remote_url" {
			continue // handled by invariant #1
		}
		t.Errorf("dashboard_session.go pipes item[%q] into the dashboard JSON "+
			"without an explicit redactor. R47-DESIGN-REMOTE-NODE-GIT-CREDENTIAL: "+
			"any field forwarded from node-cache data must pass through a "+
			"credential-stripping function, or be added to this test's whitelist "+
			"after verifying it cannot carry credentials.", key)
	}
}
