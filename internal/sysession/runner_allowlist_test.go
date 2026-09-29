package sysession

import (
	"slices"
	"testing"
)

// NewRunner applies envpolicy's Runner allowlist itself: the daemon's claude -p
// gets the Bedrock/Anthropic/proxy plumbing a session spawn gets, and nothing
// the list does not name. No caller can pass a list of its own.
func TestNewRunner_AppliesTheSysessionAllowlist(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("NAOZHI_DASHBOARD_TOKEN", "secret")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-secret")
	r, err := NewRunner(RunnerConfig{BinPath: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	env := r.(*runnerImpl).env
	for _, k := range []string{"AWS_REGION", "HTTPS_PROXY", "CLAUDE_CODE_USE_BEDROCK", "PATH"} {
		if !envHasKey(env, k) {
			t.Errorf("%s missing from the daemon env", k)
		}
	}
	for _, k := range []string{"NAOZHI_DASHBOARD_TOKEN", "SLACK_BOT_TOKEN"} {
		if envHasKey(env, k) {
			t.Errorf("%s reached the daemon env", k)
		}
	}
}

// visionBaseArgs carries the same `--setting-sources ""` as the text Runner:
// host hooks must not re-enter a daemon's claude -p.
func TestVisionBaseArgs_SettingSourcesEmpty(t *testing.T) {
	t.Parallel()
	i := slices.Index(visionBaseArgs, "--setting-sources")
	if i < 0 || i+1 >= len(visionBaseArgs) || visionBaseArgs[i+1] != "" {
		t.Fatalf("visionBaseArgs = %q, want --setting-sources followed by an empty value", visionBaseArgs)
	}
	for _, flag := range []string{"-p", "--input-format", "--output-format", "--verbose"} {
		if !slices.Contains(visionBaseArgs, flag) {
			t.Errorf("visionBaseArgs lacks %s: %q", flag, visionBaseArgs)
		}
	}
}
