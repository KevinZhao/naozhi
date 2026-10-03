package shim

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestLogFileIsLive_KeepsOwnPid: the running process's own log must never be
// swept, which is the case that matters — naozhi's shims write while naozhi runs.
func TestLogFileIsLive_KeepsOwnPid(t *testing.T) {
	t.Parallel()
	name := LogFilePrefix + itoa(os.Getpid()) + ".log"
	if !LogFileIsLive(name) {
		t.Errorf("LogFileIsLive(%q) = false for this very process", name)
	}
}

// TestLogFileIsLive_DeadPidIsSweepable uses a real exited child so the pid is
// genuinely gone rather than merely improbable.
func TestLogFileIsLive_DeadPidIsSweepable(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run /usr/bin/true: %v", err)
	}
	pid := cmd.Process.Pid
	// The child is reaped, so the pid no longer resolves — unless it was recycled
	// between Run returning and here, in which case the conservative answer (keep)
	// is the correct one and there is nothing to assert.
	name := LogFilePrefix + itoa(pid) + ".log"
	if LogFileIsLive(name) {
		t.Skipf("pid %d appears recycled; conservative keep is correct", pid)
	}
}

// TestLogFileIsLive_KeepsAnythingItDoesNotOwn: a sweep pass must not remove files
// another component (or a future naozhi) dropped in the shim state dir, and an
// unparseable pid is not evidence that a file is garbage.
func TestLogFileIsLive_KeepsAnythingItDoesNotOwn(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"shim.log",     // no pid
		"shim-.log",    // empty pid
		"shim-abc.log", // non-numeric
		"shim-0.log",   // pid 0: kill(0,…) targets a process group
		"shim--1.log",  // negative pid
		// A '+' sign: strconv.Atoi accepts it, so without the bare-digits check
		// this reaches PidAlive. The value is far past any pid_max, so PidAlive
		// says dead and the file would be swept — os.Getpid() never emits a sign.
		"shim-+99999999999999999.log",
		"something-else.log",          // not ours
		"a5ec6901ebba9c34.json",       // the shim state file
		LogFilePrefix + "123.log.bak", // wrong suffix
	} {
		if !LogFileIsLive(name) {
			t.Errorf("LogFileIsLive(%q) = false; unrecognised names must be kept", name)
		}
	}
}

// TestLogFileIsLive_ValidButDeadPidIsSweepableWhateverItsSize: the magnitude of a
// pid is not evidence of anything — a syntactically valid pid that no longer
// resolves is dead, and its log is exactly what this pass exists to remove.
func TestLogFileIsLive_ValidButDeadPidIsSweepableWhateverItsSize(t *testing.T) {
	t.Parallel()
	// Far above any OS pid_max, so it cannot be live.
	if LogFileIsLive(LogFilePrefix + "99999999999999999.log") {
		t.Error("a valid, unresolvable pid should be sweepable")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestKeyHashFileIsLive: a cli-debug/<keyhash>.log is kept exactly while the
// shim state file of the same hash names a live process, and anything this
// predicate cannot vouch for (foreign name, unreadable state) is kept too.
func TestKeyHashFileIsLive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := &Manager{stateDir: dir}
	writeState := func(hash string, pid int) {
		t.Helper()
		if err := WriteStateFile(StateFilePath(dir, hash), State{Version: 1, ShimPID: pid}); err != nil {
			t.Fatal(err)
		}
	}
	plant := func(hash, body string, perm os.FileMode) {
		t.Helper()
		path := StateFilePath(dir, hash)
		if err := os.WriteFile(path, []byte(body), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, perm); err != nil { // umask must not hide 0644
			t.Fatal(err)
		}
	}
	live := KeyHash("live")
	writeState(live, os.Getpid())
	dead := KeyHash("dead")
	writeState(dead, 999999999) // far above any pid_max
	corrupt := KeyHash("corrupt")
	plant(corrupt, "{not json", 0o600)
	insecure := KeyHash("insecure")
	plant(insecure, `{"version":1,"shim_pid":999999999}`, 0o644)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{KeyHash("no-shim") + ".log", false},
		{live + ".log", true},
		{dead + ".log", false},
		{corrupt + ".log", true},
		{insecure + ".log", true},
		{"aaaaaaaaaaaaaaaa.log", true},                       // 16 hex: not a KeyHash
		{strings.ToUpper(KeyHash("no-shim")) + ".log", true}, // KeyHash is lowercase
		{"session-notes.log", true},
		{".log", true},
	} {
		if got := m.KeyHashFileIsLive(tc.name); got != tc.want {
			t.Errorf("KeyHashFileIsLive(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
