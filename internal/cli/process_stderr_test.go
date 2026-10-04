package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"testing"
)

// newStderrTestProcess is a Process whose cli_exited handling can run: a live
// shim link (net.Pipe) for link.close and a done channel for die.
func newStderrTestProcess(t *testing.T) *Process {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	return newShimProcess(client, bufio.NewReader(client), bufio.NewWriter(client),
		&ClaudeProtocol{}, 0, 0, 0, 0)
}

// logRecords decodes the JSON log lines in buf whose msg equals msg.
func logRecords(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func feedShim(t *testing.T, p *Process, log *slog.Logger, frames ...string) {
	t.Helper()
	for _, f := range frames {
		var msg shimMsg
		if err := json.Unmarshal([]byte(f), &msg); err != nil {
			t.Fatalf("frame %s: %v", f, err)
		}
		p.handleShimMessage(msg, log)
	}
}

func TestHandleShimCLIExited_StderrTail(t *testing.T) {
	cases := []struct {
		name     string
		frames   []string
		wantTail []string
		wantWarn bool
	}{
		{
			// The shim's tail holds lines written before naozhi attached,
			// which never arrived as stderr frames, so it replaces the live tail.
			name: "frame tail wins",
			frames: []string{
				`{"type":"stderr","line":"live line"}`,
				`{"type":"cli_exited","code":1,"stderr_tail":["early 1","Error: No conversation found"]}`,
			},
			wantTail: []string{"early 1", "Error: No conversation found"},
			wantWarn: true,
		},
		{
			name: "older shim without tail keeps the live stderr",
			frames: []string{
				`{"type":"stderr","line":"loading"}`,
				`{"type":"stderr","line":"   "}`,
				`{"type":"stderr","line":"Error: Invalid API key sk-ant-abcdefghijklmnop"}`,
				`{"type":"cli_exited","code":1}`,
			},
			wantTail: []string{"loading", "Error: Invalid API key [REDACTED]"},
			wantWarn: true,
		},
		{
			name: "frame tail is sanitized, redacted and capped",
			frames: []string{
				`{"type":"cli_exited","code":2,"stderr_tail":["l1","l2","l3","l4","l5","l6","l7","l8",` +
					`"\u001b[31mError:\u001b[0m bad key sk-ant-abcdefghijklmnop"]}`,
			},
			wantTail: []string{"l2", "l3", "l4", "l5", "l6", "l7", "l8", "Error: bad key [REDACTED]"},
			wantWarn: true,
		},
		{
			name: "clean exit does not warn",
			frames: []string{
				`{"type":"stderr","line":"bye"}`,
				`{"type":"cli_exited","code":0}`,
			},
			wantTail: []string{"bye"},
			wantWarn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newStderrTestProcess(t)
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			feedShim(t, p, log, tc.frames...)

			if got := p.StderrTail(); !reflect.DeepEqual(got, tc.wantTail) {
				t.Errorf("StderrTail() = %q, want %q", got, tc.wantTail)
			}
			warns := logRecords(t, &buf, "CLI exited with error")
			if !tc.wantWarn {
				if len(warns) != 0 {
					t.Fatalf("clean exit logged %d error records: %v", len(warns), warns)
				}
				return
			}
			if len(warns) != 1 {
				t.Fatalf("got %d %q records, want 1; log:\n%s", len(warns), "CLI exited with error", buf.String())
			}
			rec := warns[0]
			if rec["level"] != "WARN" {
				t.Errorf("level = %v, want WARN", rec["level"])
			}
			raw, _ := json.Marshal(rec["stderr_tail"])
			var logged []string
			if err := json.Unmarshal(raw, &logged); err != nil || !reflect.DeepEqual(logged, tc.wantTail) {
				t.Errorf("logged stderr_tail = %s, want %q", raw, tc.wantTail)
			}
		})
	}
}

func TestShimLineReader_InitExitCarriesStderrCause(t *testing.T) {
	cases := []struct {
		name   string
		frames string
		want   string
	}{
		{
			name: "stderr frames before exit",
			frames: `{"type":"stderr","line":"Error: --mcp-config is invalid"}` + "\n" +
				`{"type":"cli_exited","code":1}` + "\n",
			want: "cli exited during init (code 1): Error: --mcp-config is invalid",
		},
		{
			name:   "tail on the exit frame",
			frames: `{"type":"cli_exited","code":3,"stderr_tail":["node: not found","more"]}` + "\n",
			want:   "cli exited during init (code 3): node: not found",
		},
		{
			name: "error line is quoted over the lines before it",
			frames: `{"type":"cli_exited","code":1,"stderr_tail":["node:internal/modules/cjs/loader:1228",` +
				`"  throw err;","Error: Cannot find module 'x'","    at Module._load (loader:1:1)"]}` + "\n",
			want: "cli exited during init (code 1): Error: Cannot find module 'x'",
		},
		{
			name:   "no stderr",
			frames: `{"type":"cli_exited","code":1}` + "\n",
			want:   "cli exited during init (code 1)",
		},
		{
			name:   "long line is capped",
			frames: `{"type":"cli_exited","code":1,"stderr_tail":["` + strings.Repeat("e", 300) + `"]}` + "\n",
			want:   "cli exited during init (code 1): " + strings.Repeat("e", stderrSummaryRunes) + "...",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Process{link: shimLink{r: bufio.NewReader(strings.NewReader(tc.frames))}}
			r := &shimLineReader{proc: p}
			data, eof, err := r.ReadLine()
			if err == nil || !eof || data != nil {
				t.Fatalf("ReadLine = (%q, %v, %v), want (nil, true, error)", data, eof, err)
			}
			if err.Error() != tc.want {
				t.Errorf("err = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}
