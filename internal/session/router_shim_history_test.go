package session

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/shim"
)

// racingHistoryLoader returns jsonl as the Claude transcript tail. When
// duringRead is set it runs first, standing in for a NewRouter startup loader
// that fills the session's history while ReconnectShimsCtx is still reading.
type racingHistoryLoader struct {
	calls      atomic.Int32
	jsonl      []clievent.EventEntry
	duringRead func()
}

func (l *racingHistoryLoader) LoadHistoryChainTail(context.Context, string, []string, string, int) []clievent.EventEntry {
	l.calls.Add(1)
	if l.duringRead != nil {
		l.duringRead()
	}
	return l.jsonl
}

func historyEntries(prefix string, n int) []clievent.EventEntry {
	out := make([]clievent.EventEntry, n)
	for i := range out {
		out[i] = clievent.EventEntry{Time: int64(1000 * (i + 1)), Type: "text", Summary: fmt.Sprintf("%s-%d", prefix, i)}
	}
	return out
}

// persistedSummaries returns the summaries in s.persistedHistory, failing the
// test if one appears twice.
func persistedSummaries(t *testing.T, s *ManagedSession) []string {
	t.Helper()
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	out := make([]string, 0, len(s.persistedHistory))
	seen := make(map[string]bool, len(s.persistedHistory))
	for _, e := range s.persistedHistory {
		if seen[e.Summary] {
			t.Errorf("persistedHistory holds %q twice", e.Summary)
		}
		seen[e.Summary] = true
		out = append(out, e.Summary)
	}
	return out
}

func summariesOf(entries []clievent.EventEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Summary
	}
	return out
}

// writeShimStateFor records state for key under dir with a socket path that
// exists, so Discover does not SIGTERM state.ShimPID (this test process).
func writeShimStateFor(t *testing.T, dir string, state shim.State) {
	t.Helper()
	if err := shim.WriteStateFile(shim.StateFilePath(dir, shim.KeyHash(state.Key)), state); err != nil {
		t.Fatal(err)
	}
}

// shortTempDir is a temp dir short enough for a unix socket path: t.TempDir()
// on darwin overflows the 104-byte sun_path limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nz-reconn-hist-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// writeDriftedShim records, under the shim state dir dir, a shim for sess
// whose argv no longer matches r's, so ReconnectShimsCtx shuts it down as
// drifted.
func writeDriftedShim(t *testing.T, dir string, r *Router, w *cli.Wrapper, sess *ManagedSession) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	socket := shim.SocketPath(shim.KeyHash(sess.key))
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stored := driftArgsFor(r).driftCompareArgs(w, "claude", sess.key, sess, &shim.SpawnOverlay{})
	writeShimStateFor(t, dir, shim.State{
		ShimPID: os.Getpid(), Socket: socket, Key: sess.key, Backend: "claude", SessionID: "sid-1",
		CLIArgs: append(stored, "--extra-flag"), SpawnOverlay: &shim.SpawnOverlay{},
	})
}

// writeLiveShim serves a fake shim for sess on a unix socket under dir (which
// must be short, see shortTempDir) and records its state, so ReconnectShimsCtx
// reattaches sess to it.
func writeLiveShim(t *testing.T, dir string, r *Router, w *cli.Wrapper, sess *ManagedSession) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	socket := shim.SocketPath(shim.KeyHash(sess.key))
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	t.Cleanup(func() {
		ln.Close()
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
		rd := bufio.NewReader(conn)
		if _, err := rd.ReadBytes('\n'); err != nil { // attach
			return
		}
		fmt.Fprintf(conn, "{\"type\":\"hello\",\"protocol_version\":%d}\n{\"type\":\"replay_done\"}\n", shim.ProtocolVersion)
		io.Copy(io.Discard, rd) //nolint:errcheck // never answer
	}()
	writeShimStateFor(t, dir, shim.State{
		ShimPID: os.Getpid(), Socket: socket, Key: sess.key, Backend: "claude", SessionID: "sid-1",
		CLIArgs:      driftArgsFor(r).driftCompareArgs(w, "claude", sess.key, sess, &shim.SpawnOverlay{}),
		SpawnOverlay: &shim.SpawnOverlay{},
	})
}

// TestReconnectShims_DriftBackfillOnlyFillsEmptyHistory drives a drifted shim
// through ReconnectShimsCtx. The JSONL backfill fills an empty session, skips
// the read for one whose history is already loaded (a startup loader or an
// earlier tick got there), and never appends onto history a startup loader
// filled while the read ran.
func TestReconnectShims_DriftBackfillOnlyFillsEmptyHistory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness")
	}
	jsonl := historyEntries("jsonl", 3)
	loaded := historyEntries("loaded", 2)
	for _, tc := range []struct {
		name       string
		preloaded  bool
		duringRead bool
		wantCalls  int32
		want       []clievent.EventEntry
	}{
		{"empty session gets the JSONL tail", false, false, 1, jsonl},
		{"history loaded before the tick is not re-read", true, false, 0, loaded},
		{"history loaded during the read is kept as is", false, true, 1, loaded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
			w.ShimManager = mgr
			loader := &racingHistoryLoader{jsonl: jsonl}
			r := NewRouter(RouterConfig{Wrapper: w, ClaudeDir: filepath.Join(dir, "claude"), HistoryLoader: loader})
			t.Cleanup(r.Shutdown)

			key := "feishu:direct:alice:general"
			sess := injectSession(r, key, nil)
			if tc.preloaded {
				sess.InjectHistory(loaded)
			}
			if tc.duringRead {
				loader.duringRead = func() { sess.InjectHistory(loaded) }
			}
			writeDriftedShim(t, dir, r, w, sess)

			r.ReconnectShimsCtx(context.Background())

			if !r.drift.has(key) {
				t.Fatal("precondition: the shim was not shut down as drifted")
			}
			if got := loader.calls.Load(); got != tc.wantCalls {
				t.Errorf("JSONL reads = %d, want %d", got, tc.wantCalls)
			}
			if got, want := persistedSummaries(t, sess), summariesOf(tc.want); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("persistedHistory = %v, want %v", got, want)
			}
		})
	}
}

// TestReconnectShims_ReconnectKeepsHistoryLoadedDuringRead reconnects a live
// shim while a startup loader fills the session's history mid-read. The
// reattached session holds that history once, with no JSONL copy appended,
// in persistedHistory and in the process event log seeded from it.
func TestReconnectShims_ReconnectKeepsHistoryLoadedDuringRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness and unix sockets")
	}
	dir := shortTempDir(t)
	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
	w.ShimManager = mgr
	loader := &racingHistoryLoader{jsonl: historyEntries("jsonl", 3)}
	r := NewRouter(RouterConfig{Wrapper: w, ClaudeDir: filepath.Join(dir, "claude"), HistoryLoader: loader})
	t.Cleanup(r.Shutdown)

	key := "feishu:direct:alice:general"
	sess := injectSession(r, key, nil)
	loaded := historyEntries("loaded", 2)
	loader.duringRead = func() { sess.InjectHistory(loaded) }

	writeLiveShim(t, dir, r, w, sess)

	r.ReconnectShimsCtx(context.Background())

	if proc, ok := sess.loadProcess().(*cli.Process); !ok || proc == nil {
		t.Fatalf("precondition: session process after reconnect = %T, want the reattached *cli.Process", sess.loadProcess())
	}
	if got := loader.calls.Load(); got != 1 {
		t.Fatalf("precondition: JSONL reads = %d, want 1", got)
	}
	want := fmt.Sprint(summariesOf(loaded))
	if got := persistedSummaries(t, sess); fmt.Sprint(got) != want {
		t.Errorf("persistedHistory = %v, want %v", got, want)
	}
	if got := summariesOf(sess.EventEntries()); fmt.Sprint(got) != want {
		t.Errorf("reattached process event log = %v, want %v", got, want)
	}
}
