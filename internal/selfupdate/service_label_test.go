package selfupdate

import (
	"strings"
	"testing"
)

// TestLaunchdServiceLabel covers the fix for a silent failure that left macOS
// deployments unable to restart at all.
//
// The package used to assume the label `naozhi install` writes
// ("com.naozhi.naozhi"). A plist created by hand or by an earlier version can
// carry any label — this project's own dev deployment uses "com.naozhi.agent".
// With a mismatched label, `launchctl list <label>` fails, ServiceRunning()
// returns false, and every restart path decides there is nothing to restart:
// no error, no warning, just a staged binary that never applies.
//
// Tests here must not run in parallel: they mutate process environment.
func TestLaunchdServiceLabel(t *testing.T) {
	t.Run("uses the label launchd injected", func(t *testing.T) {
		t.Setenv(xpcServiceNameEnv, "com.naozhi.agent")
		if got := launchdServiceLabel(); got != "com.naozhi.agent" {
			t.Fatalf("launchdServiceLabel() = %q, want the injected label com.naozhi.agent", got)
		}
	})

	t.Run("falls back to the install-time constant when unset", func(t *testing.T) {
		// The non-launchd case: `naozhi upgrade` run by hand from a terminal.
		t.Setenv(xpcServiceNameEnv, "")
		if got := launchdServiceLabel(); got != LaunchdLabel {
			t.Fatalf("launchdServiceLabel() = %q, want fallback %q", got, LaunchdLabel)
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		t.Setenv(xpcServiceNameEnv, "  com.naozhi.agent\n")
		if got := launchdServiceLabel(); got != "com.naozhi.agent" {
			t.Fatalf("launchdServiceLabel() = %q, want trimmed value", got)
		}
	})

	t.Run("whitespace-only value falls back", func(t *testing.T) {
		t.Setenv(xpcServiceNameEnv, "   ")
		if got := launchdServiceLabel(); got != LaunchdLabel {
			t.Fatalf("launchdServiceLabel() = %q, want fallback %q", got, LaunchdLabel)
		}
	})
}

// realLaunchctlListOutput is verbatim `launchctl list com.naozhi.agent` output
// from a live macOS deployment (2026-09-01). Using the real shape rather than a
// hand-written approximation is the point: the parser only has to cope with
// what launchctl actually emits.
const realLaunchctlListOutput = `{
	"StandardOutPath" = "/Users/zhaokm/.naozhi/logs/stdout.log";
	"LimitLoadToSessionType" = "Aqua";
	"StandardErrorPath" = "/Users/zhaokm/.naozhi/logs/stderr.log";
	"Label" = "com.naozhi.agent";
	"OnDemand" = false;
	"LastExitStatus" = 0;
	"PID" = 1552;
	"Program" = "/Users/zhaokm/.local/bin/naozhi";
	"ProgramArguments" = (
		"/Users/zhaokm/.local/bin/naozhi";
		"--config";
		"config-local.yaml";
	);
};`

// TestLaunchdJobRunsPath covers the check that keeps us from restarting some
// OTHER launchd job.
//
// XPC_SERVICE_NAME is inherited, so a naozhi launched by hand from a
// launchd-managed parent (Terminal.app is itself a job) reads that parent's
// label. `launchctl list <label>` succeeds for it, so mere existence proves
// nothing — we have to confirm the job runs our own binary, or `kickstart -k`
// would restart the operator's terminal.
func TestLaunchdJobRunsPath(t *testing.T) {
	t.Run("matches the Program key", func(t *testing.T) {
		if !launchdJobRunsPath(realLaunchctlListOutput, "/Users/zhaokm/.local/bin/naozhi") {
			t.Error("should have matched the Program path in real launchctl output")
		}
	})

	t.Run("rejects an unrelated job", func(t *testing.T) {
		// The Terminal.app case: a real, running job that is not us.
		other := `{
	"Label" = "com.apple.Terminal";
	"PID" = 421;
	"Program" = "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal";
};`
		if launchdJobRunsPath(other, "/Users/zhaokm/.local/bin/naozhi") {
			t.Error("must NOT match a job running a different executable — that is how `kickstart -k` ends up restarting Terminal instead of naozhi")
		}
	})

	t.Run("falls back to ProgramArguments[0]", func(t *testing.T) {
		// `naozhi install` writes ProgramArguments with no Program key.
		noProgram := `{
	"Label" = "com.naozhi.naozhi";
	"ProgramArguments" = (
		"/usr/local/bin/naozhi";
		"--config";
		"/etc/naozhi/config.yaml";
	);
};`
		if !launchdJobRunsPath(noProgram, "/usr/local/bin/naozhi") {
			t.Error("should fall back to ProgramArguments[0] when no Program key is present (the shape naozhi install writes)")
		}
	})

	t.Run("fails closed when no executable is present", func(t *testing.T) {
		// Cannot confirm ⇒ refuse. Skipping a restart we could have done is
		// recoverable; restarting the wrong service is not.
		if launchdJobRunsPath(`{ "Label" = "com.naozhi.agent"; "PID" = 1; };`, "/Users/zhaokm/.local/bin/naozhi") {
			t.Error("must fail closed when the job description carries no executable path")
		}
	})

	t.Run("empty output", func(t *testing.T) {
		if launchdJobRunsPath("", "/Users/zhaokm/.local/bin/naozhi") {
			t.Error("empty launchctl output must not be treated as a match")
		}
	})
}

// funcBody returns the text from `decl` up to the next top-level closing brace.
func funcBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("could not find %q in source", decl)
	}
	rest := src[i:]
	// A top-level func body ends at the first "\n}" — no nested block in these
	// functions is indented to column zero.
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// TestRestartLaunchdUsesKickstart and TestServiceRunningUsesResolvedLabel used to
// live here, scanning service.go for the strings "kickstart", "-k", "gui/" and
// "verifiedLaunchdLabel()". They are replaced by restart_launchd_argv_test.go,
// which asserts the argv the path actually builds (Epic I #2547).
//
// The strings could confirm the tokens were present but not that they were
// assembled correctly. Verified: rewriting the domain as system/<label> instead of
// gui/<uid>/<label> keeps every one of the four strings satisfied — including
// "gui/", which still appears in the surrounding comment — while producing an argv
// that restarts nothing. The argv test catches it at argv[3].
