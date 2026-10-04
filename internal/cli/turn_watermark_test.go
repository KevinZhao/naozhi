package cli

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

func TestTurnWatermark_RoundTrip(t *testing.T) {
	w := TurnWatermark{ShimPID: 4242, Seq: 17}
	got, ok := ParseTurnWatermark(w.String())
	if !ok || got != w {
		t.Fatalf("ParseTurnWatermark(%q) = (%+v, %v), want (%+v, true)", w.String(), got, ok, w)
	}
	for _, s := range []string{"", "4242", "x:1", "0:1", "-1:1", "4242:", "4242:-1", "4242:1:2"} {
		if got, ok := ParseTurnWatermark(s); ok {
			t.Errorf("ParseTurnWatermark(%q) = (%+v, true), want ok=false", s, got)
		}
	}
}

// TestTurnWatermark_NeedsTheShimPID: seqs are numbered per shim, so a seq
// from a shim that never named itself places nothing.
func TestTurnWatermark_NeedsTheShimPID(t *testing.T) {
	p := &Process{}
	p.link.lastSeq.Store(9)
	if w, ok := p.TurnWatermark(); ok {
		t.Errorf("TurnWatermark = (%+v, true) with no shim PID, want ok=false", w)
	}
	p.link.shimPID = 4242
	if w, ok := p.TurnWatermark(); !ok || w != (TurnWatermark{ShimPID: 4242, Seq: 9}) {
		t.Errorf("TurnWatermark = (%+v, %v), want ({4242 9}, true)", w, ok)
	}
}

// TestAdoptableAfter covers the gate cron adopts by. A replayed result is
// adopted only with evidence that it came after the run's Send: a reconnect
// replays the whole backlog, so on an idle shim that result is the previous
// turn's, which the old process already delivered.
func TestAdoptableAfter(t *testing.T) {
	const pid = 4242
	finished := &clievent.Event{Type: "result", SubType: "success", Result: "done", SessionID: "s1"}
	cases := []struct {
		name     string
		shimPID  int
		midTurn  bool
		finished *clievent.Event
		seq      int64
		resolve  bool // mid-turn only: the late result has been latched
		w        TurnWatermark
		known    bool
		want     bool
	}{
		{name: "unarmed", shimPID: pid, w: TurnWatermark{pid, 0}, known: true, want: false},
		{name: "mid turn pending, no watermark", shimPID: pid, midTurn: true, want: true},
		{name: "mid turn resolved, no watermark", shimPID: pid, midTurn: true, resolve: true, want: true},
		{name: "result past the watermark", shimPID: pid, finished: finished, seq: 8, w: TurnWatermark{pid, 5}, known: true, want: true},
		{name: "result at the watermark is stale", shimPID: pid, finished: finished, seq: 8, w: TurnWatermark{pid, 8}, known: true, want: false},
		{name: "result before the watermark is stale", shimPID: pid, finished: finished, seq: 3, w: TurnWatermark{pid, 8}, known: true, want: false},
		{name: "result from a shim started since", shimPID: pid, finished: finished, seq: 2, w: TurnWatermark{pid + 1, 100}, known: true, want: true},
		{name: "result with no watermark", shimPID: pid, finished: finished, seq: 8, want: false},
		{name: "result on a shim that never named itself", finished: finished, seq: 8, w: TurnWatermark{pid, 5}, known: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Process{}
			p.link.shimPID = tc.shimPID
			p.applyReconnectVerdict(tc.midTurn, tc.finished, tc.seq)
			if tc.resolve {
				p.adopted.resolveResult(*finished)
			}
			if got := p.AdoptableAfter(tc.w, tc.known); got != tc.want {
				t.Errorf("AdoptableAfter(%+v, %v) = %v, want %v", tc.w, tc.known, got, tc.want)
			}
		})
	}
}
