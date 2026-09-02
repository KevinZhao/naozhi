package selfupdate

import (
	"os"
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

// TestRestartLaunchdUsesKickstart is a source-level regression gate.
//
// restartLaunchd cannot be executed in a test — it would restart the developer's
// own naozhi. What CAN be locked down is that it never returns to the
// unload/load shape, which is broken by construction for a self-restart:
// `launchctl unload` removes the job whose process is making the call, so the
// following `load` runs in a race against our own SIGTERM (or never runs),
// leaving the service stopped rather than restarted.
//
// Reading our own source is unusual, but the alternative is no coverage at all
// on a fix whose failure mode is "the service silently stays down".
func TestRestartLaunchdUsesKickstart(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	body := restartLaunchdBody(t, string(src))

	if !strings.Contains(body, `"kickstart", "-k"`) {
		t.Error("restartLaunchd must restart via `launchctl kickstart -k` so launchd, not this dying process, owns the restart")
	}
	if strings.Contains(body, `"unload"`) || strings.Contains(body, `"load"`) {
		t.Error("restartLaunchd must not use launchctl unload/load: unload removes the job whose own process is making the call, so the service ends up stopped instead of restarted")
	}
	if !strings.Contains(body, "verifiedLaunchdLabel()") {
		t.Error("restartLaunchd must use verifiedLaunchdLabel(): an unverified inherited XPC_SERVICE_NAME can name a completely different launchd job")
	}
	if !strings.Contains(body, "gui/") {
		t.Error("restartLaunchd must target the gui/<uid> domain, which is where naozhi install writes its LaunchAgent")
	}
}

// TestServiceRunningUsesResolvedLabel guards the other half of the same fix.
// Correcting restartLaunchd alone would not help: ServiceRunning() gates it,
// so a stale constant there keeps every restart a silent no-op.
func TestServiceRunningUsesResolvedLabel(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	body := funcBody(t, string(src), "func ServiceRunning()")
	if !strings.Contains(body, "verifiedLaunchdLabel()") {
		t.Error("ServiceRunning must resolve the launchd label via verifiedLaunchdLabel(); the install-time constant makes restarts a silent no-op wherever the plist label differs, and an unverified label can report a foreign job as ours")
	}
}

// restartLaunchdBody extracts restartLaunchd's body from the source.
func restartLaunchdBody(t *testing.T, src string) string {
	t.Helper()
	return funcBody(t, src, "func restartLaunchd()")
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
