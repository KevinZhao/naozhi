package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
)

// StartupFailure is true only for a CLI that exited with a status of its own
// before any output, and naozhi did not end it first.
func TestProcess_StartupFailure(t *testing.T) {
	initFrame := stdoutFrame(1, `{"type":"system","subtype":"init","session_id":"s1"}`)
	exit := func(code int) string {
		return fmt.Sprintf(`{"type":"cli_exited","code":%d,"stderr_tail":["No conversation found with session ID: abc"]}`, code)
	}
	cases := []struct {
		name      string
		reason    string // a death reason stamped before the frames
		frames    []string
		wantOK    bool
		wantClass clierr.ExitClass
	}{
		{name: "exit 1 with no output", frames: []string{exit(1)}, wantOK: true, wantClass: clierr.ExitResumeNotFound},
		{name: "claude's startup error result, then exit 1", frames: []string{stdoutFrame(1, startupRejectResult), exit(1)},
			wantOK: true, wantClass: clierr.ExitResumeNotFound},
		{name: "exit 1 after output", frames: []string{initFrame, exit(1)}},
		{name: "exit 0", frames: []string{exit(0)}},
		{name: "signal", frames: []string{exit(-1)}},
		{name: "naozhi ended it first", reason: DeathReasonKilled, frames: []string{exit(1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh := newPassthroughShim(t)
			defer sh.close()
			if class, _, ok := sh.proc.StartupFailure(); ok {
				t.Fatalf("StartupFailure() of a live process = %d, true", class)
			}
			if tc.reason != "" {
				sh.proc.setDeathReason(tc.reason)
			}
			before := time.Now()
			go sh.proc.readLoop()
			for _, f := range tc.frames {
				sh.srv.SendFrame(f)
			}
			select {
			case <-sh.proc.done:
			case <-time.After(3 * time.Second):
				t.Fatal("readLoop did not end after cli_exited")
			}
			class, at, ok := sh.proc.StartupFailure()
			if ok != tc.wantOK || class != tc.wantClass {
				t.Fatalf("StartupFailure() = %d, %v; want %d, %v", class, ok, tc.wantClass, tc.wantOK)
			}
			if ok && (at.Before(before) || at.After(time.Now())) {
				t.Errorf("failure time %v is not when the CLI exited", at)
			}
		})
	}
}

// A resume step the backend answered with an error or died on is a verdict on
// the session; a timeout or a broken connection is not.
func TestResumeRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"acp rpc error", fmt.Errorf("%w -32603: session not found", ErrACPRPC), true},
		{"codex rpc error", fmt.Errorf("%w -32600: no rollout", ErrCodexRPC), true},
		{"cli exited", fmt.Errorf("read ACP response: %w", initExitError(shimMsg{}, []string{"panic: bad transcript"})), true},
		{"timeout", fmt.Errorf("%w (id=1)", ErrACPTimeout), false},
		{"connection", fmt.Errorf("read ACP response: %w", io.ErrUnexpectedEOF), false},
	}
	for _, tc := range cases {
		err := resumeRejected(tc.err)
		if got := errors.Is(err, clierr.ErrResumeRejected); got != tc.want {
			t.Errorf("%s: errors.Is(resumeRejected(%v), ErrResumeRejected) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: resumeRejected dropped the cause %v", tc.name, tc.err)
		}
	}
}

// Init's resume step reports a refused session as ErrResumeRejected, for ACP
// and codex alike; the same refusal of a fresh session is not one.
func TestInit_ResumeRefusalIsResumeRejected(t *testing.T) {
	t.Parallel()
	const refusal = `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"session not found"}}`
	cases := []struct {
		name   string
		proto  Protocol
		resume string
		lines  []string
		want   bool
	}{
		{"acp session/load", &ACPProtocol{}, "sess-1", []string{`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1}}`, refusal}, true},
		{"acp session/new", &ACPProtocol{}, "", []string{`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1}}`, refusal}, false},
		{"codex thread/resume", &CodexProtocol{}, "thread-1", []string{`{"jsonrpc":"2.0","id":0,"result":{}}`, refusal}, true},
		{"codex thread/start", &CodexProtocol{}, "", []string{`{"jsonrpc":"2.0","id":0,"result":{}}`, refusal}, false},
	}
	for _, tc := range cases {
		var w bytes.Buffer
		_, err := tc.proto.Init(&JSONRW{W: &w, R: &mockLineReader{lines: tc.lines}}, tc.resume, "/tmp/work")
		if err == nil {
			t.Fatalf("%s: Init succeeded on a refusal", tc.name)
		}
		if got := errors.Is(err, clierr.ErrResumeRejected); got != tc.want {
			t.Errorf("%s: Init err = %v; ErrResumeRejected = %v, want %v", tc.name, err, got, tc.want)
		}
	}
}
