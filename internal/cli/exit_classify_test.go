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

func TestClassifyExit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		code int64
		tail []string
		want clierr.ExitClass
	}{
		// Captured from claude-code 2.1.288's stderr on a fatal exit.
		{"stale resume id", 1, []string{"No conversation found with session ID: 0f3c2a7e-1111-4222-8333-444455556666"}, clierr.ExitResumeNotFound},
		{"missing mcp config file", 1, []string{"Error: Invalid MCP configuration:", "MCP config file not found: /nonexistent/m.json"}, clierr.ExitMCPConfig},
		{"unparseable mcp config", 1, []string{"Error: Invalid MCP configuration:", "MCP config is not a valid JSON"}, clierr.ExitMCPConfig},
		// A broken --settings file is only a warning; the CLI runs on.
		{"settings warning then the cause", 1, []string{settingsMissingWarning, "No conversation found with session ID: abc"}, clierr.ExitResumeNotFound},
		{"missing settings warning alone", 1, []string{settingsMissingWarning}, clierr.ExitUnknown},
		{"unparseable settings warning alone", 1, []string{"claude: warning: failed to merge user --settings: failed to parse JSONC: key must be a string at line 1 column 2; applying managed settings only"}, clierr.ExitUnknown},
		{"invalid api key", 1, []string{"Invalid API key · Please run /login"}, clierr.ExitAuth},
		{"expired oauth token", 1, []string{"OAuth token has expired. Please obtain a new token or refresh your existing token."}, clierr.ExitAuth},
		{"api 401", 1, []string{`API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`}, clierr.ExitAuth},
		{"acp backend not logged in", 1, []string{"Error: You are not logged in, please log in with kiro-cli login"}, clierr.ExitAuth},
		{"login required", 1, []string{"Error: login required: run `codex login`"}, clierr.ExitAuth},
		{"node missing", 1, []string{"env: node: No such file or directory"}, clierr.ExitMissingRuntime},
		{"broken install", 1, []string{"node:internal/modules/cjs/loader:1228", "  throw err;", "Error: Cannot find module '/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js'"}, clierr.ExitMissingRuntime},
		{"binary missing", 1, []string{"/bin/sh: 1: claude: command not found"}, clierr.ExitMissingRuntime},
		// The first matching class wins, whatever line it is on.
		{"resume beats auth", 1, []string{"Error: Invalid API key", "No conversation found with session ID: abc"}, clierr.ExitResumeNotFound},
		// A pair of terms counts only on one line.
		{"pair split across lines", 1, []string{"Loading MCP servers", "Error: invalid flag --frobnicate"}, clierr.ExitUnknown},
		{"stack line number", 1, []string{"TypeError: Cannot read properties of undefined (reading 'id')", "    at run (/app/cli.js:401:15)"}, clierr.ExitUnknown},
		{"no stderr", 1, nil, clierr.ExitUnknown},
		// dash and busybox say "not found" without "command"; a POSIX shell
		// exits 127 when it cannot find a command.
		{"dash sh missing runtime", 1, []string{"sh: 1: node: not found"}, clierr.ExitMissingRuntime},
		{"dash sh missing runtime, 127", 127, []string{"sh: 1: node: not found"}, clierr.ExitMissingRuntime},
		{"dash exec missing runtime", 1, []string{"/usr/local/bin/claude: 3: exec: node: not found"}, clierr.ExitMissingRuntime},
		{"busybox missing runtime, 127", 127, []string{"/usr/local/bin/claude: line 3: node: not found"}, clierr.ExitMissingRuntime},
		{"busybox line without 127", 1, []string{"/usr/local/bin/claude: line 3: node: not found"}, clierr.ExitUnknown},
		{"127 with no stderr", 127, nil, clierr.ExitMissingRuntime},
		{"127 after a warning only", 127, []string{settingsMissingWarning}, clierr.ExitMissingRuntime},
		// A class the text names wins over 127.
		{"stale resume id at 127", 127, []string{"No conversation found with session ID: abc"}, clierr.ExitResumeNotFound},
		{"invalid api key at 127", 127, []string{"Invalid API key · Please run /login"}, clierr.ExitAuth},
		{"mcp config not found at 127", 127, []string{"Error: Invalid MCP configuration:", "MCP config file not found: /x"}, clierr.ExitMCPConfig},
		{"model not found", 1, []string{"Error: model: not found"}, clierr.ExitUnknown},
		// "sh:" and "exec:" count only as whole words.
		{"word ending in sh", 1, []string{"Error: token refresh: not found"}, clierr.ExitUnknown},
		{"bash word", 1, []string{"bash: node: not found"}, clierr.ExitUnknown},
		{"word ending in exec", 1, []string{"Error: noexec: not found"}, clierr.ExitUnknown},
		{"bin sh missing runtime", 1, []string{"/bin/sh: 1: node: not found"}, clierr.ExitMissingRuntime},
		{"not found split from sh", 1, []string{"sh: 1: starting", "Error: model: not found"}, clierr.ExitUnknown},
		{"exit code 2", 2, nil, clierr.ExitUnknown},
	}
	for _, tc := range cases {
		if got := classifyExit(tc.code, tc.tail); got != tc.want {
			t.Errorf("%s: classifyExit(%d, %q) = %d, want %d", tc.name, tc.code, tc.tail, got, tc.want)
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

// An error_during_execution result after the CLI is running, with no
// priority:"now" message behind it, is no preemption: the pending slot stays
// queued, and the exit that follows answers it with the classified exit error.
func TestSendPassthrough_ErrorAfterInitWithoutUrgentWaitsForExit(t *testing.T) {
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
	sh.srv.SendFrame(`{"type":"cli_exited","code":1}`)
	select {
	case err := <-errCh:
		var pe *clierr.ProcessExitedError
		if !errors.As(err, &pe) || pe.Code != 1 {
			t.Errorf("pending send err = %v, want the code-1 ProcessExitedError", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pending send did not return after cli_exited")
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

// A shell's 127 names a missing runtime only before the CLI wrote stdout.
func TestRecordExit_Code127(t *testing.T) {
	t.Parallel()
	for _, sawOutput := range []bool{false, true} {
		p := &Process{}
		p.sawOutput.Store(sawOutput)
		p.recordExit(127, nil)
		want := clierr.ExitMissingRuntime
		if sawOutput {
			want = clierr.ExitUnknown
		}
		var pe *clierr.ProcessExitedError
		if !errors.As(p.exitErr(), &pe) || pe.Code != 127 || pe.Class != want {
			t.Errorf("sawOutput=%v: exitErr() = %v (%+v), want a code-127 exit of class %d", sawOutput, p.exitErr(), pe, want)
		}
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
