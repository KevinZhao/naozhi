package cli

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
)

func TestClassifyStderr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tail []string
		want clierr.ExitClass
	}{
		{"stale resume id", []string{"No conversation found with session ID: 0f3c2a7e-4c1d-4b7a-9e1f-2d3c4b5a6f70"}, clierr.ExitResumeNotFound},
		{"invalid mcp config", []string{"Error: Invalid MCP configuration:", "mcpServers.x: Does not adhere to MCP server configuration schema"}, clierr.ExitMCPConfig},
		{"missing mcp config file", []string{"Error: MCP config file not found: /etc/naozhi/mcp.json"}, clierr.ExitMCPConfig},
		{"unparseable settings", []string{"Error: Failed to parse settings file /home/u/.claude/settings.json"}, clierr.ExitInvalidSettings},
		{"missing settings file", []string{"Error: Settings file not found: /tmp/x.json"}, clierr.ExitInvalidSettings},
		{"invalid api key", []string{"Invalid API key · Please run /login"}, clierr.ExitAuth},
		{"expired oauth token", []string{"OAuth token has expired. Please obtain a new token or refresh your existing token."}, clierr.ExitAuth},
		{"api 401", []string{`API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`}, clierr.ExitAuth},
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
