package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
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
		{"cli exited non-zero", fmt.Errorf("read ACP response: %w", initExitError(shimMsg{Code: shimMsgCode{Value: 1, Present: true}}, []string{"Error: authentication failed"})), true},
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

// A failed Init handshake is a clierr.ErrSpawnInit whose text is the log
// line; a CLI that exited during it also carries the exit and the class its
// stderr names, before or after its first answer, and an exit 0 names none.
func TestInitHandshake_FailureIsSpawnInit(t *testing.T) {
	t.Parallel()
	const (
		acpInit   = `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1}}`
		codexInit = `{"jsonrpc":"2.0","id":0,"result":{}}`
		authExit  = `{"type":"cli_exited","code":1,"stderr_tail":["Error: authentication failed"]}`
		loginExit = `{"type":"cli_exited","code":2,"stderr_tail":["You are not logged in. Run kiro-cli login."]}`
	)
	cases := []struct {
		name         string
		proto        Protocol
		resume       string
		frames       []string
		wantPrefix   string
		wantSuffix   string
		wantExit     bool
		wantClass    clierr.ExitClass // checked when wantTyped
		wantTyped    bool
		wantRejected bool
	}{
		{name: "acp exits before initialize answers", proto: &ACPProtocol{}, frames: []string{authExit},
			wantPrefix: "protocol init: acp initialize: ", wantSuffix: "cli exited during init (code 1): Error: authentication failed",
			wantExit: true, wantTyped: true, wantClass: clierr.ExitAuth},
		{name: "acp exits on session/new", proto: &ACPProtocol{}, frames: []string{stdoutFrame(1, acpInit), loginExit},
			wantPrefix: "protocol init: acp session/new: ", wantSuffix: "cli exited during init (code 2): You are not logged in. Run kiro-cli login.",
			wantExit: true, wantTyped: true, wantClass: clierr.ExitAuth},
		{name: "acp exits on session/load", proto: &ACPProtocol{}, resume: "sess-1", frames: []string{stdoutFrame(1, acpInit), authExit},
			wantPrefix: "protocol init: acp session/load: ", wantSuffix: "cli exited during init (code 1): Error: authentication failed",
			wantExit: true, wantTyped: true, wantClass: clierr.ExitAuth, wantRejected: true},
		{name: "codex exits on thread/start", proto: &CodexProtocol{}, frames: []string{stdoutFrame(1, codexInit),
			`{"type":"cli_exited","code":1,"stderr_tail":["env: node: No such file or directory"]}`},
			wantPrefix: "protocol init: codex thread/start: ", wantSuffix: "cli exited during init (code 1): env: node: No such file or directory",
			wantExit: true, wantTyped: true, wantClass: clierr.ExitMissingRuntime},
		{name: "exit 0 names no cause", proto: &ACPProtocol{}, frames: []string{`{"type":"cli_exited","code":0,"stderr_tail":["Error: authentication failed"]}`},
			wantPrefix: "protocol init: acp initialize: ", wantSuffix: "cli exited during init (code 0): Error: authentication failed",
			wantExit: true},
		{name: "rpc error is no exit", proto: &ACPProtocol{}, frames: []string{stdoutFrame(1, `{"jsonrpc":"2.0","id":0,"error":{"code":-32600,"message":"invalid request"}}`)},
			wantPrefix: "protocol init: acp initialize: ", wantSuffix: "invalid request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := shimTestPair(tc.proto)
			t.Cleanup(func() { srv.Close(); p.link.conn.Close() })
			go io.Copy(io.Discard, srv.conn) //nolint:errcheck // drains the handshake requests
			go func() {
				for _, f := range tc.frames {
					srv.SendFrame(f)
				}
			}()

			_, err := initHandshake(tc.proto, p, tc.resume, "/tmp/work")
			if err == nil {
				t.Fatal("initHandshake succeeded")
			}
			if msg := err.Error(); !strings.HasPrefix(msg, tc.wantPrefix) || !strings.HasSuffix(msg, tc.wantSuffix) {
				t.Errorf("err = %q, want %q...%q", msg, tc.wantPrefix, tc.wantSuffix)
			}
			if !errors.Is(err, clierr.ErrSpawnInit) {
				t.Errorf("err = %v, want ErrSpawnInit", err)
			}
			if got := errors.Is(err, clierr.ErrProcessExited); got != tc.wantExit {
				t.Errorf("errors.Is(err, ErrProcessExited) = %v, want %v", got, tc.wantExit)
			}
			if got := errors.Is(err, errInitCLIExited); got != tc.wantExit {
				t.Errorf("errors.Is(err, errInitCLIExited) = %v, want %v", got, tc.wantExit)
			}
			var pe *clierr.ProcessExitedError
			if got := errors.As(err, &pe); got != tc.wantTyped {
				t.Fatalf("errors.As(err, *ProcessExitedError) = %v (%+v), want %v", got, pe, tc.wantTyped)
			}
			if tc.wantTyped && pe.Class != tc.wantClass {
				t.Errorf("class = %d, want %d", pe.Class, tc.wantClass)
			}
			if got := errors.Is(err, clierr.ErrResumeRejected); got != tc.wantRejected {
				t.Errorf("errors.Is(err, ErrResumeRejected) = %v, want %v", got, tc.wantRejected)
			}
		})
	}
}
