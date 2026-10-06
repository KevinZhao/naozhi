package workflow_test

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/workflow"
)

// benchObserve decodes and observes snapshots in turn on one Tracker: the
// read loop's cost per snapshot, board wake excluded.
func benchObserve(b *testing.B, lines []string, fresh bool) {
	var proto cli.ClaudeProtocol
	tr := workflow.New(nil)
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if fresh {
			tr = workflow.New(nil)
		}
		events, _, err := proto.ReadEvent(lines[i%len(lines)])
		if err != nil || len(events) != 1 {
			b.Fatal(err)
		}
		tr.Observe(&events[0], now)
	}
}

// steadyLines are consecutive snapshots that differ in their running rows
// only, as CC's do between agent starts and ends.
func steadyLines(n int, eq bool) []string {
	var out []string
	for tick := 1; tick <= 4; tick++ {
		out = append(out, string(bigSnapshot(n, bigOpts{eqSummary: eq, running: 3, tick: tick})))
	}
	return out
}

func BenchmarkObserve_Snapshot398(b *testing.B) { benchObserve(b, steadyLines(398, false), false) }

func BenchmarkObserve_Snapshot2000EqFirst(b *testing.B) {
	benchObserve(b, steadyLines(2000, true)[:1], true)
}

func BenchmarkObserve_Snapshot2000EqSteady(b *testing.B) {
	benchObserve(b, steadyLines(2000, true), false)
}

// BenchmarkDecode_* are the decode alone: subtract them from the
// matching Observe benchmark for the Tracker's share.
func BenchmarkDecode_Snapshot398(b *testing.B) { benchDecode(b, steadyLines(398, false)[0]) }

func BenchmarkDecode_Snapshot2000Eq(b *testing.B) { benchDecode(b, steadyLines(2000, true)[0]) }

func benchDecode(b *testing.B, line string) {
	var proto cli.ClaudeProtocol
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := proto.ReadEvent(line); err != nil {
			b.Fatal(err)
		}
	}
}
