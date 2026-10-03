//go:build unix

package shim

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// stateDirSnapshot records every entry's name, size and mtime so a test can
// assert a scan left the directory byte-for-byte alone.
func stateDirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := make(map[string]string, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		snap[e.Name()] = fmt.Sprintf("%s/%d", info.ModTime().Format(time.RFC3339Nano), info.Size())
	}
	return snap
}

func writeTestState(t *testing.T, dir string, st State) string {
	t.Helper()
	path := filepath.Join(dir, KeyHash(st.Key)+".json")
	if err := WriteStateFile(path, st); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInspect_ReadOnly: one state file of every kind Discover would delete or
// signal, and Inspect must classify each while leaving the directory and the
// live PID (this test process) untouched — whichever binary is calling.
func TestInspect_ReadOnly(t *testing.T) {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGUSR2)
	defer signal.Stop(sigCh)

	dir := t.TempDir()
	sock := filepath.Join(dir, "live.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("bad json {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	stranded := filepath.Join(dir, ".0123abcd.json.928115277.tmp")
	if err := os.WriteFile(stranded, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-strandedTempAge - time.Minute)
	if err := os.Chtimes(stranded, old, old); err != nil {
		t.Fatal(err)
	}
	dead := writeTestState(t, dir, State{ShimPID: 999999999, Socket: sock, Key: "t:dead"})
	live := writeTestState(t, dir, State{ShimPID: os.Getpid(), Socket: sock, Key: "t:live"})
	noSock := writeTestState(t, dir, State{ShimPID: os.Getpid(), Socket: filepath.Join(dir, "gone.sock"), Key: "t:nosock"})

	for _, tc := range []struct {
		name      string
		naozhiBin string // "" keeps the test binary, which is what os.Getpid() runs
		want      map[string]StateVerdict
	}{
		{"same binary", "", map[string]StateVerdict{
			corrupt: StateCorrupt, dead: StateDeadPID, live: StateLive, noSock: StateSocketMissing,
		}},
		{"foreign binary", "/nonexistent/not-naozhi", map[string]StateVerdict{
			corrupt: StateCorrupt, dead: StateDeadPID, live: StateBinaryMismatch, noSock: StateBinaryMismatch,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mustNewManager(t, ManagerConfig{StateDir: dir})
			if tc.naozhiBin != "" {
				m.naozhiBin = tc.naozhiBin
			}
			before := stateDirSnapshot(t, dir)
			got, err := m.Inspect()
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if after := stateDirSnapshot(t, dir); len(after) != len(before) {
				t.Fatalf("Inspect changed the state dir: before %v, after %v", before, after)
			} else {
				for name, v := range before {
					if after[name] != v {
						t.Fatalf("Inspect changed %s: before %q, after %q", name, v, after[name])
					}
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Inspect returned %d entries, want %d (temp files skipped): %+v", len(got), len(tc.want), got)
			}
			for _, e := range got {
				if e.IdentityErr != nil {
					t.Skipf("binary identity unavailable here: %v", e.IdentityErr)
				}
				if want, ok := tc.want[e.Path]; !ok || e.Verdict != want {
					t.Errorf("%s: verdict %d, want %d (known %v)", filepath.Base(e.Path), e.Verdict, want, ok)
				}
			}
		})
	}

	select {
	case sig := <-sigCh:
		t.Fatalf("Inspect signalled the live PID: %v", sig)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestDiscover_RemovesStateWithBinaryMismatch pins the service-side hygiene
// Inspect deliberately skips: a live PID running another binary is treated as
// PID reuse, so its state file goes and the PID is not signalled.
func TestDiscover_RemovesStateWithBinaryMismatch(t *testing.T) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGUSR2)
	defer signal.Stop(sigCh)

	dir := t.TempDir()
	sock := filepath.Join(dir, "live.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTestState(t, dir, State{ShimPID: os.Getpid(), Socket: sock, Key: "t:foreign"})

	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	m.naozhiBin = "/nonexistent/not-naozhi"
	states, _, err := m.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("Discover returned a binary-mismatch shim: %+v", states)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("binary-mismatch state file survived Discover: stat = %v", err)
	}
	select {
	case sig := <-sigCh:
		t.Fatalf("Discover signalled a PID it could not identify: %v", sig)
	case <-time.After(300 * time.Millisecond):
	}
}
