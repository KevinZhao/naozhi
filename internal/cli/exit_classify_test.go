package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
)

const settingsMissingWarning = "claude: warning: failed to merge user --settings: failed to read /nonexistent/s.json: No such file or directory (os error 2); applying managed settings only"

func TestClassifyStderr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tail []string
		want clierr.ExitClass
	}{
		// Captured from claude-code 2.1.288's stderr on a fatal exit.
		{"stale resume id", []string{"No conversation found with session ID: 0f3c2a7e-1111-4222-8333-444455556666"}, clierr.ExitResumeNotFound},
		{"missing mcp config file", []string{"Error: Invalid MCP configuration:", "MCP config file not found: /nonexistent/m.json"}, clierr.ExitMCPConfig},
		{"unparseable mcp config", []string{"Error: Invalid MCP configuration:", "MCP config is not a valid JSON"}, clierr.ExitMCPConfig},
		// A broken --settings file is only a warning; the CLI runs on.
		{"settings warning then the cause", []string{settingsMissingWarning, "No conversation found with session ID: abc"}, clierr.ExitResumeNotFound},
		{"missing settings warning alone", []string{settingsMissingWarning}, clierr.ExitUnknown},
		{"unparseable settings warning alone", []string{"claude: warning: failed to merge user --settings: failed to parse JSONC: key must be a string at line 1 column 2; applying managed settings only"}, clierr.ExitUnknown},
		{"invalid api key", []string{"Invalid API key · Please run /login"}, clierr.ExitAuth},
		{"expired oauth token", []string{"OAuth token has expired. Please obtain a new token or refresh your existing token."}, clierr.ExitAuth},
		{"api 401", []string{`API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`}, clierr.ExitAuth},
		{"acp backend not logged in", []string{"Error: You are not logged in, please log in with kiro-cli login"}, clierr.ExitAuth},
		{"login required", []string{"Error: login required: run `codex login`"}, clierr.ExitAuth},
		{"node missing", []string{"env: node: No such file or directory"}, clierr.ExitMissingRuntime},
		{"broken install", []string{"node:internal/modules/cjs/loader:1228", "  throw err;", "Error: Cannot find module '/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js'"}, clierr.ExitMissingRuntime},
		{"binary missing", []string{"/bin/sh: 1: claude: command not found"}, clierr.ExitMissingRuntime},
		// The first matching class wins, whatever line it is on.
		{"resume beats auth", []string{"Error: Invalid API key", "No conversation found with session ID: abc"}, clierr.ExitResumeNotFound},
		// A pair of terms counts only on one line.
		{"pair split across lines", []string{"Loading MCP servers", "Error: invalid flag --frobnicate"}, clierr.ExitUnknown},
		{"stack line number", []string{"TypeError: Cannot read properties of undefined (reading 'id')", "    at run (/app/cli.js:401:15)"}, clierr.ExitUnknown},
		{"no stderr", nil, clierr.ExitUnknown},
	}
	for _, tc := range cases {
		if got := classifyStderr(tc.tail); got != tc.want {
			t.Errorf("%s: classifyStderr(%q) = %d, want %d", tc.name, tc.tail, got, tc.want)
		}
	}
}

// SendFrame writes one raw shim frame.
func (s *shimTestServer) SendFrame(frame string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writer.WriteString(frame + "\n") //nolint:errcheck
	s.writer.Flush()                   //nolint:errcheck
}

// A send that finds the CLI gone gets an error saying how it exited: the
// pending turn's caller (legacy Send and passthrough alike), and every send
// after it.
func TestSend_ExitErrorCarriesStderrClass(t *testing.T) {
	const staleResume = `{"type":"cli_exited","code":1,"stderr_tail":["No conversation found with session ID: abc"]}`
	cases := []struct {
		name        string
		passthrough bool
		frames      []string
		wantTyped   bool
		wantClass   clierr.ExitClass
		wantDetail  string
	}{
		{name: "startup failure, passthrough", passthrough: true, frames: []string{staleResume},
			wantTyped: true, wantClass: clierr.ExitResumeNotFound, wantDetail: "No conversation found with session ID: abc"},
		{name: "startup failure, legacy send", frames: []string{staleResume},
			wantTyped: true, wantClass: clierr.ExitResumeNotFound, wantDetail: "No conversation found with session ID: abc"},
		// The real CLI writes an error result to stdout before it exits on a
		// stale --resume id; that result is the failure, not output.
		{name: "startup failure as claude reports it", passthrough: true, frames: []string{
			stdoutFrame(1, startupRejectResult),
			`{"type":"cli_exited","code":1,"stderr_tail":["` + settingsMissingWarning + `","No conversation found with session ID: abc"]}`,
		}, wantTyped: true, wantClass: clierr.ExitResumeNotFound, wantDetail: "No conversation found with session ID: abc"},
		{name: "live stderr frames from an older shim", passthrough: true, frames: []string{
			`{"type":"stderr","line":"Error: Invalid API key · Please run /login"}`,
			`{"type":"cli_exited","code":1}`,
		}, wantTyped: true, wantClass: clierr.ExitAuth, wantDetail: "Error: Invalid API key · Please run /login"},
		// Once the CLI has written stdout its stderr is not a startup cause.
		{name: "exit after output", passthrough: true, frames: []string{
			`{"type":"stdout","seq":1,"line":"{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s1\"}"}`,
			staleResume,
		}, wantTyped: true, wantClass: clierr.ExitUnknown, wantDetail: "No conversation found with session ID: abc"},
		{name: "clean exit", passthrough: true, frames: []string{
			`{"type":"cli_exited","code":0,"stderr_tail":["No conversation found with session ID: abc"]}`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh := newPassthroughShim(t)
			defer sh.close()
			go sh.proc.readLoop()

			errCh := make(chan error, 1)
			go func() {
				var err error
				if tc.passthrough {
					_, err = sh.proc.SendPassthrough(context.Background(), "hi", nil, nil, "")
				} else {
					_, err = sh.proc.Send(context.Background(), "hi", nil, nil)
				}
				errCh <- err
			}()
			_ = sh.expectWrite(t, 2*time.Second)
			for _, f := range tc.frames {
				sh.srv.SendFrame(f)
			}

			var errs []error
			select {
			case err := <-errCh:
				errs = append(errs, err)
			case <-time.After(3 * time.Second):
				t.Fatal("the pending send did not return after cli_exited")
			}
			<-sh.proc.done
			_, err := sh.proc.SendPassthrough(context.Background(), "again", nil, nil, "")
			errs = append(errs, err)
			_, err = sh.proc.Send(context.Background(), "again", nil, nil)
			errs = append(errs, err)

			for i, err := range errs {
				if !errors.Is(err, clierr.ErrProcessExited) {
					t.Fatalf("send %d: err = %v, want ErrProcessExited", i, err)
				}
				var pe *clierr.ProcessExitedError
				if got := errors.As(err, &pe); got != tc.wantTyped {
					t.Fatalf("send %d: errors.As(%v, *ProcessExitedError) = %v, want %v", i, err, got, tc.wantTyped)
				}
				if tc.wantTyped && (pe.Code != 1 || pe.Class != tc.wantClass) {
					t.Errorf("send %d: exit error = %+v, want code 1 class %d", i, *pe, tc.wantClass)
				}
			}
			if got := sh.proc.DeathDetail(); got != tc.wantDetail {
				t.Errorf("DeathDetail() = %q, want %q", got, tc.wantDetail)
			}
		})
	}
}

// startupRejectResult is the result claude-code 2.1.288 writes to stdout
// before it exits 1 on a stale --resume id (usage trimmed).
const startupRejectResult = `{"type":"result","subtype":"error_during_execution","duration_ms":0,"is_error":true,"num_turns":0,"session_id":"abc","total_cost_usd":0,"errors":["No conversation found with session ID: abc"]}`

// stdoutFrame wraps one CLI stdout line in a shim stdout frame.
func stdoutFrame(seq int, line string) string {
	b, _ := json.Marshal(map[string]any{"type": "stdout", "seq": seq, "line": line})
	return string(b)
}

// An error_during_execution result after the CLI is running is a preemption:
// a pending slot the CLI never replayed was dropped, and its caller is told so.
func TestSendPassthrough_PreemptedAfterInitIsAbortedByUrgent(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	errCh := make(chan error, 1)
	go func() {
		_, err := sh.proc.SendPassthrough(context.Background(), "hi", nil, nil, "")
		errCh <- err
	}()
	_ = sh.expectWrite(t, 2*time.Second)
	sh.srv.SendFrame(stdoutFrame(1, `{"type":"system","subtype":"init","session_id":"abc"}`))
	sh.srv.SendFrame(stdoutFrame(2, startupRejectResult))
	select {
	case err := <-errCh:
		if !errors.Is(err, clierr.ErrAbortedByUrgent) {
			t.Errorf("pending send err = %v, want ErrAbortedByUrgent", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pending send was not aborted by the preempting result")
	}
}

// An Init handshake line is stdout too: a CLI that answered the handshake and
// then exited is past startup, so its stderr names no startup cause.
func TestShimLineReader_InitStdoutCountsAsOutput(t *testing.T) {
	t.Parallel()
	p := &Process{link: shimLink{r: bufio.NewReader(strings.NewReader(`{"type":"stdout","line":"{}"}` + "\n"))}}
	if _, _, err := (&shimLineReader{proc: p}).ReadLine(); err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	p.recordExit(1, []string{"No conversation found with session ID: abc"})
	var pe *clierr.ProcessExitedError
	if !errors.As(p.exitErr(), &pe) || pe.Class != clierr.ExitUnknown {
		t.Errorf("exitErr() = %v (%+v), want a code-1 exit with no class", p.exitErr(), pe)
	}
}

// A reattached CLI ran before this naozhi attached, so its exit is not a
// startup failure even when nothing was in flight.
func TestApplyReconnectVerdict_PastStartup(t *testing.T) {
	t.Parallel()
	p := &Process{}
	p.applyReconnectVerdict(false, nil, 0)
	p.recordExit(1, []string{"No conversation found with session ID: abc"})
	var pe *clierr.ProcessExitedError
	if !errors.As(p.exitErr(), &pe) || pe.Class != clierr.ExitUnknown {
		t.Errorf("exitErr() = %v (%+v), want a code-1 exit with no class", p.exitErr(), pe)
	}
}
