package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsGitHubURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"https github", "https://github.com/foo/bar.git", true},
		{"https github no .git", "https://github.com/foo/bar", true},
		{"ssh scp github", "git@github.com:foo/bar.git", true},
		{"ssh proto github", "ssh://git@github.com/foo/bar.git", true},
		{"https github uppercase", "https://GitHub.com/foo/bar.git", true},
		{"gitlab https", "https://gitlab.com/foo/bar.git", false},
		{"gitlab ssh", "git@gitlab.com:foo/bar.git", false},
		{"self-hosted", "https://git.example.com/foo/bar.git", false},
		{"empty", "", false},
		{"bogus", "not-a-url", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isGitHubURL(tc.url)
			if got != tc.want {
				t.Errorf("isGitHubURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func writeGitConfig(t *testing.T, dir, body string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDetectGitHubRemote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		config     string
		wantURL    string
		wantGitHub bool
	}{
		{
			name: "origin github https",
			config: `[core]
	repositoryformatversion = 0
[remote "origin"]
	url = https://github.com/foo/bar.git
	fetch = +refs/heads/*:refs/remotes/origin/*
`,
			wantURL:    "https://github.com/foo/bar.git",
			wantGitHub: true,
		},
		{
			name: "origin ssh github",
			config: `[remote "origin"]
	url = git@github.com:foo/bar.git
`,
			wantURL:    "git@github.com:foo/bar.git",
			wantGitHub: true,
		},
		{
			name: "origin gitlab",
			config: `[remote "origin"]
	url = https://gitlab.com/foo/bar.git
`,
			wantURL:    "https://gitlab.com/foo/bar.git",
			wantGitHub: false,
		},
		{
			name: "origin preferred over first",
			config: `[remote "upstream"]
	url = https://gitlab.com/foo/bar.git
[remote "origin"]
	url = https://github.com/foo/bar.git
`,
			wantURL:    "https://github.com/foo/bar.git",
			wantGitHub: true,
		},
		{
			name: "fallback to first remote",
			config: `[remote "upstream"]
	url = https://github.com/foo/bar.git
`,
			wantURL:    "https://github.com/foo/bar.git",
			wantGitHub: true,
		},
		{
			name:       "no remote",
			config:     "[core]\n\trepositoryformatversion = 0\n",
			wantURL:    "",
			wantGitHub: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeGitConfig(t, dir, tc.config)
			gotURL, gotGH := DetectGitHubRemote(dir)
			if gotURL != tc.wantURL || gotGH != tc.wantGitHub {
				t.Errorf("DetectGitHubRemote = (%q, %v), want (%q, %v)", gotURL, gotGH, tc.wantURL, tc.wantGitHub)
			}
		})
	}
}

func TestDetectGitHubRemote_NoGitDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gotURL, gotGH := DetectGitHubRemote(dir)
	if gotURL != "" || gotGH {
		t.Errorf("expected empty result for non-git dir, got (%q, %v)", gotURL, gotGH)
	}
}
