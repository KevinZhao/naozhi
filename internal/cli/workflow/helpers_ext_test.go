package workflow_test

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
)

// probeFixture is a CC 2.1.288 stream-json capture of a 3-agent workflow
// (phase Ask runs A and B in parallel, phase Sum runs C), home paths
// rewritten to /home/u. docs/rfc/workflow-dashboard.md §1.2.1 walks it.
const (
	probeFixture = "testdata/probe-3agent.jsonl"
	probeResult  = "testdata/run/wf_2997921d-435.json"
	probeTask    = "w113pvmto"
	probeSession = "04a8fc10-6fa5-4b8e-82ba-621974425917"
)

// probeLines returns the capture's lines; probeLines(t)[n-1] is line n.
func probeLines(tb testing.TB) []string {
	tb.Helper()
	f, err := os.Open(probeFixture)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}
	if len(lines) != 20 {
		tb.Fatalf("%s: %d lines, want the 20 of the capture", probeFixture, len(lines))
	}
	return lines
}

// countingDecoder is the real claude decoder, counting the lines it decodes.
type countingDecoder struct {
	proto cli.ClaudeProtocol
	n     atomic.Int64
}

func (d *countingDecoder) ReadEvent(line string) ([]clievent.Event, bool, error) {
	d.n.Add(1)
	return d.proto.ReadEvent(line)
}

// feed decodes lines and observes them live, one millisecond apart from t0.
func feed(tb testing.TB, tr *workflow.Tracker, t0 time.Time, lines ...string) {
	tb.Helper()
	var proto cli.ClaudeProtocol
	for i, line := range lines {
		events, _, err := proto.ReadEvent(line)
		if err != nil {
			tb.Fatalf("decode: %v\n%.200s", err, line)
		}
		for j := range events {
			tr.Observe(&events[j], t0.Add(time.Duration(i)*time.Millisecond))
		}
	}
}

func only(tb testing.TB, tr *workflow.Tracker) *workflow.Workflow {
	tb.Helper()
	set := tr.Load()
	if len(set.Workflows) != 1 {
		tb.Fatalf("Set holds %d workflows, want 1: %+v", len(set.Workflows), set.Workflows)
	}
	return set.Workflows[0]
}

// bigOpts shapes bigSnapshot's synthetic line.
type bigOpts struct {
	task string // task id; default "wbig00001"
	// eqSummary puts an '=' in every lastToolSummary, sending RedactSecrets
	// down its env-assignment regex.
	eqSummary bool
	// running is how many of the highest-index agents are still running;
	// tick varies their progress so consecutive snapshots differ in them only.
	running, tick int
}

// bigSnapshot synthesizes a task_progress line with n agents shaped like a
// real batch workflow (previews included, ~1.2KB per agent).
func bigSnapshot(n int, o bigOpts) []byte {
	if o.task == "" {
		o.task = "wbig00001"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"system","subtype":"task_progress","task_id":%q,"tool_use_id":"toolu_big","description":"Implement: impl:1.0","usage":{"total_tokens":%d,"tool_uses":2,"duration_ms":3},"last_tool_name":"impl:1.0","summary":"batch","workflow_progress":[`, o.task, 1000+o.tick)
	phases := []string{"Implement", "Review", "Fix", "Merge", "Close", "Wrap-up"}
	for i, p := range phases {
		fmt.Fprintf(&b, `{"type":"workflow_phase","index":%d,"title":%q},`, i+1, p)
	}
	prompt := strings.Repeat("You are executing one PR of a batch that fixes triaged review issues. ", 6)
	result := strings.Repeat(`{\"status\":\"ready\",\"pr_number\":3125,\"branch\":\"fix/x\"}`, 7)
	summary := "ready"
	if o.eqSummary {
		summary = "GOTOOLCHAIN=go1.26.6 go test -run=TestX ./internal/cli/..."
	}
	for i := 1; i <= n; i++ {
		state, tokens, tool := "done", 29976, summary
		if i > n-o.running {
			state, tokens, tool = "progress", 100*o.tick, fmt.Sprintf("%s step %d", summary, o.tick)
		}
		errField := ""
		if i%17 == 0 {
			errField = `"error":"agent exited without calling StructuredOutput",`
		}
		fmt.Fprintf(&b, `{"type":"workflow_agent","index":%d,"label":"impl:%d.0","phaseIndex":%d,"phaseTitle":%q,"agentId":"a%016x","model":"claude-opus-5-5[1m]","state":%q,"startedAt":1791029368736,"queuedAt":1791029368726,"attempt":1,"lastToolName":"Bash","lastToolSummary":%q,%s"promptPreview":%q,"promptFramed":true,"lastProgressAt":1791029804410,"tokens":%d,"toolCalls":12,"durationMs":435671,"resultPreview":"%s"}`,
			i, i, i%6+1, phases[i%6], i, state, tool, errField, prompt, tokens, result)
		if i < n {
			b.WriteByte(',')
		}
	}
	fmt.Fprintf(&b, `],"uuid":"46226038-ae5d-45c3-a33a-45868db9aa27","session_id":%q}`, probeSession)
	return []byte(b.String())
}
