package selfupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// agentExec fakes launchctl and plutil for the LaunchAgent fallback: `plutil
// -convert json` answers from plistJSON by file name, `launchctl list <label>`
// from loaded (missing ⇒ the job is not loaded). Every argv is recorded.
type agentExec struct {
	plistJSON map[string]string
	loaded    map[string]string
	calls     [][]string
	// realPlutil runs the real plutil instead of answering from plistJSON.
	realPlutil bool
}

func (f *agentExec) command(name string, args ...string) *exec.Cmd {
	f.calls = append(f.calls, append([]string{name}, args...))
	switch {
	case len(args) == 2 && args[0] == "list":
		if out, ok := f.loaded[args[1]]; ok {
			return exec.Command("printf", "%s", out)
		}
		return exec.Command("false")
	case len(args) == 5 && args[0] == "-convert" && f.realPlutil:
		return exec.Command(name, args...)
	case len(args) == 5 && args[0] == "-convert":
		if out, ok := f.plistJSON[filepath.Base(args[4])]; ok {
			return exec.Command("printf", "%s", out)
		}
		return exec.Command("false")
	}
	return exec.Command("true")
}

func (f *agentExec) listed(label string) bool {
	for _, c := range f.calls {
		if len(c) == 3 && c[1] == "list" && c[2] == label {
			return true
		}
	}
	return false
}

func (f *agentExec) kickstart() []string {
	for _, c := range f.calls {
		if len(c) > 1 && c[1] == "kickstart" {
			return c
		}
	}
	return nil
}

// withLaunchAgents points launchAgentsDir at a directory holding the given
// file names (contents are irrelevant: plutil is faked) and installs f.
func withLaunchAgents(t *testing.T, f *agentExec, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	origDir := launchAgentsDir
	launchAgentsDir = func() string { return dir }
	t.Cleanup(func() { launchAgentsDir = origDir })
	origExec := execCommand
	execCommand = f.command
	t.Cleanup(func() { execCommand = origExec })
}

func listOutput(label, program string) string {
	return fmt.Sprintf("{\n\t\"Label\" = %q;\n\t\"PID\" = 3051;\n\t\"Program\" = %q;\n};", label, program)
}

// The case `naozhi upgrade` hit: run from a shell, so no XPC_SERVICE_NAME of
// the service's, and the job's label (com.naozhi.agent) is not the
// install-time constant. The job must still be found through the LaunchAgent
// that runs this binary, and only after launchctl confirms it.
func TestServiceLaunchdLabel_FindsTheLaunchAgentRunningThisBinary(t *testing.T) {
	self, err := SelfPath()
	if err != nil {
		t.Skipf("SelfPath unavailable: %v", err)
	}
	t.Setenv(xpcServiceNameEnv, "")
	f := &agentExec{
		plistJSON: map[string]string{
			"com.apple.other.plist": `{"Label":"com.apple.other","Program":"/usr/bin/true"}`,
			// The shape `naozhi install` and the live plist use: no Program key.
			"com.naozhi.agent.plist": fmt.Sprintf(`{"Label":"com.naozhi.agent","ProgramArguments":[%q,"--config","config-local.yaml"]}`, self),
		},
		loaded: map[string]string{
			"com.apple.other":  listOutput("com.apple.other", "/usr/bin/true"),
			"com.naozhi.agent": listOutput("com.naozhi.agent", self),
		},
	}
	withLaunchAgents(t, f, "com.apple.other.plist", "com.naozhi.agent.plist", "com.naozhi.agent.plist.bak")

	if got := serviceLaunchdLabel(); got != "com.naozhi.agent" {
		t.Fatalf("serviceLaunchdLabel() = %q, want com.naozhi.agent; calls=%v", got, f.calls)
	}
	if f.listed("com.apple.other") {
		t.Error("asked launchctl about a job whose plist runs another program")
	}
	if verifiedLaunchdLabel() != "" {
		t.Error("verifiedLaunchdLabel must stay the job THIS process runs under; the scan is for the external upgrader only")
	}

	if runtime.GOOS != "darwin" {
		return
	}
	if !ServiceRunning() {
		t.Error("ServiceRunning() = false with com.naozhi.agent running this binary — `naozhi upgrade` prints \"Service not running\" and skips the restart")
	}
	f.calls = nil
	if err := RestartService(context.Background()); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/com.naozhi.agent", os.Getuid())}
	got := f.kickstart()
	if len(got) != 1+len(want) {
		t.Fatalf("kickstart argv = %v, want [<launchctl> %v]; calls=%v", got, want, f.calls)
	}
	for i, w := range want {
		if got[i+1] != w {
			t.Errorf("kickstart argv[%d] = %q, want %q", i+1, got[i+1], w)
		}
	}
	if target := LaunchdServiceTarget(); target != want[2] {
		t.Errorf("LaunchdServiceTarget() = %q, want %q", target, want[2])
	}
}

// Fail closed: a plist naming this binary proves nothing until launchd says
// the loaded job runs it. Not loaded, or loaded with another program (the
// plist was edited after load), means no label and no restart.
func TestServiceLaunchdLabel_PlistAloneIsNotEnough(t *testing.T) {
	self, err := SelfPath()
	if err != nil {
		t.Skipf("SelfPath unavailable: %v", err)
	}
	plist := map[string]string{
		"com.naozhi.agent.plist": fmt.Sprintf(`{"Label":"com.naozhi.agent","Program":%q}`, self),
	}
	for name, loaded := range map[string]map[string]string{
		"not loaded":             {},
		"loaded with other path": {"com.naozhi.agent": listOutput("com.naozhi.agent", "/opt/other/naozhi")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(xpcServiceNameEnv, "")
			f := &agentExec{plistJSON: plist, loaded: loaded}
			withLaunchAgents(t, f, "com.naozhi.agent.plist")
			if got := serviceLaunchdLabel(); got != "" {
				t.Fatalf("serviceLaunchdLabel() = %q, want \"\"", got)
			}
			if runtime.GOOS == "darwin" {
				if ServiceRunning() {
					t.Error("ServiceRunning() = true for an unconfirmed job")
				}
				if err := RestartService(context.Background()); err != nil {
					t.Fatalf("RestartService: %v", err)
				}
				if argv := f.kickstart(); argv != nil {
					t.Errorf("kickstarted an unconfirmed job: %v", argv)
				}
			}
		})
	}
}

// The label launchd gave this process still wins: no scan when it verifies.
func TestServiceLaunchdLabel_PrefersTheVerifiedOwnLabel(t *testing.T) {
	self, err := SelfPath()
	if err != nil {
		t.Skipf("SelfPath unavailable: %v", err)
	}
	t.Setenv(xpcServiceNameEnv, "com.naozhi.mine")
	f := &agentExec{
		plistJSON: map[string]string{"com.naozhi.agent.plist": fmt.Sprintf(`{"Label":"com.naozhi.agent","Program":%q}`, self)},
		loaded: map[string]string{
			"com.naozhi.mine":  listOutput("com.naozhi.mine", self),
			"com.naozhi.agent": listOutput("com.naozhi.agent", self),
		},
	}
	withLaunchAgents(t, f, "com.naozhi.agent.plist")
	if got := serviceLaunchdLabel(); got != "com.naozhi.mine" {
		t.Fatalf("serviceLaunchdLabel() = %q, want com.naozhi.mine", got)
	}
	for _, c := range f.calls {
		if len(c) > 1 && c[1] == "-convert" {
			t.Errorf("scanned LaunchAgents although the own label verified: %v", c)
		}
	}
}

// The plutil argv against a real XML plist of the shape `naozhi install`
// writes, so the fallback is not only proven against canned JSON.
func TestLaunchAgentLabelsRunning_RealPlutil(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is macOS only")
	}
	self, err := SelfPath()
	if err != nil {
		t.Skipf("SelfPath unavailable: %v", err)
	}
	// A symlink to self in the plist must still match.
	link := filepath.Join(t.TempDir(), "naozhi")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	f := &agentExec{realPlutil: true}
	withLaunchAgents(t, f)
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>com.naozhi.agent</string>
	<key>ProgramArguments</key>
	<array><string>%s</string><string>--config</string><string>config.yaml</string></array>
	<key>KeepAlive</key><true/>
</dict>
</plist>
`, link)
	if err := os.WriteFile(filepath.Join(launchAgentsDir(), "com.naozhi.agent.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	got := launchAgentLabelsRunning(self)
	if len(got) != 1 || got[0] != "com.naozhi.agent" {
		t.Fatalf("launchAgentLabelsRunning = %v, want [com.naozhi.agent]; calls=%v", got, f.calls)
	}
}
