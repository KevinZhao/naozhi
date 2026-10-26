package project

import (
	"os"
	"testing"
)

// TestIsRestrictedDeniedName_Unit pins the suffix/substring/prefix matchers
// independently of any HTTP plumbing so future edits to the lists are caught
// even when the handler glue would otherwise short-circuit. R20260527122801-SEC-6
// (#1330): the deny-list is the last line of defence when a sensitive /tmp
// artefact is world-readable (e.g. 0o777 ssh-agent socket); regressions here
// silently re-open the disclosure path.
func TestIsRestrictedDeniedName_Unit(t *testing.T) {
	denied := []string{
		"/tmp/ssh-agent.4567",
		"/tmp/postgres.sock",
		"/tmp/redis.sock",
		"/tmp/nginx.pid",
		"/tmp/core.5678",
		"/tmp/crash.report",
		"/tmp/CRASH.dump", // case-insensitive
		"/tmp/MyAgentSSH", // substring
		// gpg-agent artefacts (R20260602190132-SEC-4)
		"/tmp/S.gpg-agent",
		"/tmp/S.gpg-agent.extra",
		"/tmp/S.gpg-agent.ssh",
		"/tmp/GPG-SOCKET", // case-insensitive gpg substring
		// X11 authority artefacts
		"/tmp/.Xauthority", // mixed case — isRestrictedDeniedName lowercases
		"/tmp/.xauthority",
		// D-Bus artefacts
		"/tmp/.dbus-keyrings",
		"/tmp/.dbus",
	}
	for _, name := range denied {
		if !isRestrictedDeniedName(name) {
			t.Errorf("isRestrictedDeniedName(%q) = false; want true", name)
		}
	}

	allowed := []string{
		"/tmp/log.txt",
		"/tmp/build-output.json",
		"/tmp/notes.md",
		"/tmp/some.config",
		"/tmp/", // edge: empty basename
		"",
	}
	for _, name := range allowed {
		if isRestrictedDeniedName(name) {
			t.Errorf("isRestrictedDeniedName(%q) = true; want false", name)
		}
	}
}

// TestIsRestrictedIrregularType_Unit pins the non-regular-type gate
// (R090031-SEC-7 / #1688) independently of HTTP plumbing. A regular file is
// allowed; sockets / FIFOs / device nodes are refused. We synthesize the
// mode via a fake os.FileInfo so the test is portable and doesn't need to
// mknod a device.
func TestIsRestrictedIrregularType_Unit(t *testing.T) {
	cases := []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{"regular", 0o644, false},
		{"regular-world-rw", 0o666, false},
		{"socket", 0o666 | os.ModeSocket, true},
		{"named-pipe", 0o666 | os.ModeNamedPipe, true},
		{"device", 0o666 | os.ModeDevice, true},
		{"char-device", 0o666 | os.ModeDevice | os.ModeCharDevice, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isRestrictedIrregularType(fakeModeInfo{mode: tc.mode})
			if got != tc.want {
				t.Errorf("isRestrictedIrregularType(mode=%v) = %v; want %v", tc.mode, got, tc.want)
			}
		})
	}
}

// fakeModeInfo is a minimal os.FileInfo exposing only Mode(), enough for
// isRestrictedIrregularType's mode-bit check.
type fakeModeInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (f fakeModeInfo) Mode() os.FileMode { return f.mode }
