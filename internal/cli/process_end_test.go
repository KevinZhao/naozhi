package cli

import (
	"bufio"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

func endTestFrame(id string, out int) string {
	return `{"type":"assistant","message":{"id":"` + id + `","role":"assistant","model":"opus",` +
		`"content":[{"type":"text","text":"x"}],"usage":{"input_tokens":2,"output_tokens":` + strconv.Itoa(out) + `}}}`
}

// endRecorder collects every ProcessEnd delivered to it.
type endRecorder struct{ ch chan ProcessEnd }

func newEndRecorder() *endRecorder { return &endRecorder{ch: make(chan ProcessEnd, 4)} }

func (r *endRecorder) fn(e ProcessEnd) { r.ch <- e }

func (r *endRecorder) one(t *testing.T) ProcessEnd {
	t.Helper()
	select {
	case e := <-r.ch:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("ProcessEnd not delivered")
	}
	return ProcessEnd{}
}

func (r *endRecorder) none(t *testing.T) {
	t.Helper()
	select {
	case e := <-r.ch:
		t.Fatalf("ProcessEnd delivered again: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// A process that dies hands its end to the hook once: the usage of the turn
// after the last result, that result's receive time, and its attach time.
func TestProcess_EndDeliveredOnceWithUsageSinceLastResult(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	rec := newEndRecorder()
	p.SetOnEnd(rec.fn)
	p.startReadLoop()
	p.turn.mu.Lock()
	p.turn.sessionID = "sid-1"
	p.turn.mu.Unlock()

	srv.SendStdout(endTestFrame("msg_1", 5))
	srv.SendStdout(`{"type":"result","subtype":"success","result":"ok","session_id":"sid-1","total_cost_usd":0.1}`)
	srv.SendStdout(endTestFrame("msg_2", 7))
	testhelper.Eventually(t, func() bool { return len(p.eventLog.EntriesSince(0)) >= 3 }, 2*time.Second, "frames not logged")
	srv.Close()

	e := rec.one(t)
	if e.Detached || e.SessionID != "sid-1" {
		t.Fatalf("end = %+v, want a non-detached end of sid-1", e)
	}
	if want := []clievent.ShadowModel{{Model: "opus", Input: 2, Output: 7}}; !reflect.DeepEqual(e.Shadow.Models, want) {
		t.Fatalf("shadow = %+v, want only the turn after the result %+v", e.Shadow.Models, want)
	}
	if e.StartedAt.IsZero() || e.LastResultAt.Before(e.StartedAt.Truncate(time.Millisecond)) || e.EndedAt.Before(e.LastResultAt) {
		t.Fatalf("times started=%v lastResult=%v ended=%v, want started <= lastResult <= ended", e.StartedAt, e.LastResultAt, e.EndedAt)
	}
	p.SetOnEnd(rec.fn) // a rebind after delivery (rename) gets nothing
	rec.none(t)
}

// Detach lets go of a CLI that keeps running: the end says so and carries no
// usage, which the CLI will still report itself.
func TestProcess_DetachEndCarriesNoUsage(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	rec := newEndRecorder()
	p.SetOnEnd(rec.fn)
	p.startReadLoop()
	srv.SendStdout(endTestFrame("msg_1", 5))
	testhelper.Eventually(t, func() bool { return len(p.eventLog.EntriesSince(0)) >= 1 }, 2*time.Second, "frame not logged")

	p.Detach()
	e := rec.one(t)
	if !e.Detached || !e.Shadow.IsZero() {
		t.Fatalf("end = %+v, want detached with no usage", e)
	}
}

// A hook bound after the process already ended still receives the end, once.
func TestProcess_EndDeliveredToHookSetAfterIt(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()
	srv.Close()
	testhelper.Eventually(t, func() bool { return !p.Alive() }, 2*time.Second, "read loop did not exit")

	rec := newEndRecorder()
	p.SetOnEnd(rec.fn)
	rec.one(t)
	p.SetOnEnd(rec.fn)
	rec.none(t)
}

// childPID starts a process standing in for the shim and returns its PID;
// with exited set it is already reaped, so the PID is dead.
func childPID(t *testing.T, exited bool) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if exited {
		cmd = exec.Command("true")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("no child process: %v", err)
	}
	if exited {
		_ = cmd.Wait()
		return cmd.Process.Pid
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// Only a socket that breaks under a shim still running is ShimLive: the
// reconcile loop reattaches that CLI, which then reports the spend itself.
// The CLI's exit, naozhi's Kill or Close, and a dead shim all end the CLI.
func TestProcess_EndShimLiveOnlyWhenTheShimOutlivesTheSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX child process for the shim PID")
	}
	for _, tc := range []struct {
		name     string
		deadShim bool
		readsOwn bool // the end reads the shim side itself
		end      func(p *Process, srv *shimTestServer)
		want     bool
	}{
		{"socket broke, shim alive", false, false, func(_ *Process, srv *shimTestServer) { srv.Close() }, true},
		{"socket broke, shim dead", true, false, func(_ *Process, srv *shimTestServer) { srv.Close() }, false},
		{"cli exited", false, false, func(_ *Process, srv *shimTestServer) { srv.SendCLIExited(1); srv.Close() }, false},
		{"killed", false, false, func(p *Process, _ *shimTestServer) { p.Kill() }, false},
		{"closed", false, true, func(p *Process, srv *shimTestServer) {
			// The shim answers shutdown by hanging up without cli_exited.
			go func() {
				rd := bufio.NewReader(srv.conn)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					if strings.Contains(line, `"shutdown"`) {
						srv.Close()
						return
					}
				}
			}()
			p.Close()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := shimTestPair(&ClaudeProtocol{})
			p.link.shimPID = childPID(t, tc.deadShim)
			if !tc.readsOwn {
				startServerDrain(srv)
			}
			rec := newEndRecorder()
			p.SetOnEnd(rec.fn)
			p.startReadLoop()
			srv.SendStdout(endTestFrame("msg_1", 5))
			testhelper.Eventually(t, func() bool { return len(p.eventLog.EntriesSince(0)) >= 1 }, 2*time.Second, "frame not logged")

			tc.end(p, srv)
			if e := rec.one(t); e.ShimLive != tc.want {
				t.Fatalf("ShimLive = %v, want %v (death reason %q)", e.ShimLive, tc.want, p.DeathReason())
			}
		})
	}
}
