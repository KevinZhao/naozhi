package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// restart_launchd_argv_test.go — asserts the argv the macOS restart path actually
// builds, replacing four strings.Contains checks over service.go. Epic I #2547.
//
// The strings were "kickstart", "-k", "gui/" and "verifiedLaunchdLabel()". They
// confirm those tokens appear in restartLaunchd's body; they cannot confirm the
// argv is assembled from them correctly, nor that the label reaching launchctl is
// the VERIFIED one. That distinction is the whole bug this path was fixed for: a
// label naming some other job makes `launchctl list` fail, verifiedLaunchdLabel
// return "", and the restart become a silent no-op.

// fakeExec replaces execCommand for one test, recording every argv and answering
// `launchctl list` with a canned plist so the label verification can succeed
// hermetically. `true` is used as the program so the returned *exec.Cmd is
// runnable without touching launchctl.
type fakeExec struct {
	t        *testing.T
	listOut  string
	listFail bool
	calls    [][]string
}

func (f *fakeExec) command(name string, args ...string) *exec.Cmd {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) > 0 && args[0] == "list" {
		if f.listFail {
			return exec.Command("false")
		}
		// `echo` the canned plist so .Output() returns it.
		return exec.Command("printf", "%s", f.listOut)
	}
	return exec.Command("true")
}

func (f *fakeExec) argvFor(sub string) []string {
	for _, c := range f.calls {
		for _, a := range c {
			if a == sub {
				return c
			}
		}
	}
	return nil
}

func withFakeExec(t *testing.T, f *fakeExec) {
	t.Helper()
	orig := execCommand
	execCommand = f.command
	t.Cleanup(func() { execCommand = orig })
}

// TestRestartLaunchd_ArgvIsKickstartOnTheVerifiedLabel is the behavioural
// replacement for TestRestartLaunchdUsesKickstart.
func TestRestartLaunchd_ArgvIsKickstartOnTheVerifiedLabel(t *testing.T) {
	self, err := SelfPath()
	if err != nil {
		t.Skipf("SelfPath unavailable: %v", err)
	}
	const label = "com.naozhi.agent"
	t.Setenv(xpcServiceNameEnv, label)
	// The plist must name THIS test binary, or launchdJobRunsPath rejects it and
	// verifiedLaunchdLabel returns "" — which is the silent no-op under test.
	f := &fakeExec{t: t, listOut: fmt.Sprintf("{\n\t\"Label\" = %q;\n\t\"PID\" = 1552;\n\t\"Program\" = %q;\n};", label, self)}
	withFakeExec(t, f)

	if err := restartLaunchd(); err != nil {
		t.Fatalf("restartLaunchd: %v", err)
	}

	argv := f.argvFor("kickstart")
	if argv == nil {
		t.Fatalf("no launchctl kickstart call was made; calls=%v — a restart that issues no command is the silent no-op this path was fixed for", f.calls)
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)}
	if len(argv) != 1+len(want) {
		t.Fatalf("argv = %v, want [<launchctl> %s]", argv, strings.Join(want, " "))
	}
	if !strings.HasSuffix(argv[0], "launchctl") {
		t.Errorf("argv[0] = %q, want the launchctl binary", argv[0])
	}
	for i, w := range want {
		if argv[i+1] != w {
			t.Errorf("argv[%d] = %q, want %q (full argv %v)", i+1, argv[i+1], w, argv)
		}
	}
	// unload/load would remove the job whose own process is making the call, so the
	// service ends up stopped rather than restarted.
	for _, c := range f.calls {
		for _, a := range c {
			if a == "unload" || a == "load" {
				t.Errorf("restartLaunchd issued %q: unload/load stops the job instead of restarting it; calls=%v", a, f.calls)
			}
		}
	}
}

// TestRestartLaunchd_UnverifiedLabelIssuesNoCommand pins the OTHER half: when the
// job that answers to the inherited label runs a different executable — the
// Terminal.app case, since XPC_SERVICE_NAME is inherited — restartLaunchd must
// issue no kickstart at all rather than restarting the operator's terminal.
func TestRestartLaunchd_UnverifiedLabelIssuesNoCommand(t *testing.T) {
	t.Setenv(xpcServiceNameEnv, "com.apple.Terminal")
	f := &fakeExec{t: t, listOut: "{\n\t\"Label\" = \"com.apple.Terminal\";\n\t\"PID\" = 421;\n\t\"Program\" = \"/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal\";\n};"}
	withFakeExec(t, f)

	if err := restartLaunchd(); err != nil {
		t.Fatalf("restartLaunchd should be a no-op, got %v", err)
	}
	if argv := f.argvFor("kickstart"); argv != nil {
		t.Errorf("issued %v against a job running a different executable — this is how `kickstart -k` restarts Terminal instead of naozhi", argv)
	}
}

// TestServiceRunning_UsesTheVerifiedLabel replaces
// TestServiceRunningUsesResolvedLabel's text check: with the label unverifiable,
// ServiceRunning must report false, because that is what gates every restart.
func TestServiceRunning_UsesTheVerifiedLabel(t *testing.T) {
	t.Setenv(xpcServiceNameEnv, "com.naozhi.agent")
	f := &fakeExec{t: t, listFail: true}
	withFakeExec(t, f)
	if ServiceRunning() {
		t.Error("ServiceRunning() = true while `launchctl list` fails; a restart gated on this would act on an unverified label")
	}
}
