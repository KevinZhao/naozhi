package meeting

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// script writes an executable shell script standing in for naozhi-audio.
func script(t *testing.T, body string) ExecHelper {
	t.Helper()
	p := filepath.Join(t.TempDir(), "naozhi-audio")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return ExecHelper{Path: p, StopGrace: 200 * time.Millisecond, StallAfter: time.Minute}
}

type recordResult struct {
	ev  Event
	err error
}

// record runs h.Record; started is closed at the recorder's first line (a new executable can be
// slow to start on macOS, so tests do not time the stop from the call).
func record(h ExecHelper, stop <-chan struct{}) (res <-chan recordResult, started <-chan struct{}) {
	out, first := make(chan recordResult, 1), make(chan struct{})
	var once sync.Once
	go func() {
		ev, err := h.Record(context.Background(), "", []string{"b"}, 1, stop, func(Event) { once.Do(func() { close(first) }) })
		out <- recordResult{ev, err}
	}()
	return out, first
}

func wait(t *testing.T, res <-chan recordResult) recordResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("Record did not return")
		return recordResult{}
	}
}

func TestRecordStopsCleanly(t *testing.T) {
	t.Parallel()
	h := script(t, `echo '{"event":"started"}'; read x; echo '{"event":"stopped"}'`)
	h.StopGrace = 5 * time.Second
	stop := make(chan struct{})
	res, started := record(h, stop)
	<-started
	close(stop)
	r := wait(t, res)
	if r.err != nil || r.ev.Event != "stopped" || r.ev.ExitCode != 0 {
		t.Fatalf("got %+v", r)
	}
}

// A recorder stuck before it watches stdin (coreaudiod not answering after a meeting program
// crashed) is killed once the stop's grace runs out — with the child it re-spawned, which holds
// stdout too.
func TestRecordKillsRecorderIgnoringStop(t *testing.T) {
	t.Parallel()
	h := script(t, `echo '{"event":"started"}'; sleep 600 & wait`)
	stop := make(chan struct{})
	res, started := record(h, stop)
	<-started
	close(stop)
	if r := wait(t, res); !errors.Is(r.err, errRecorderHung) {
		t.Fatalf("got %+v, want errRecorderHung", r)
	}
}

func TestRecordKillsSilentRecorder(t *testing.T) {
	t.Parallel()
	h := script(t, `sleep 600 & wait`)
	h.StallAfter = 300 * time.Millisecond
	res, _ := record(h, make(chan struct{}))
	if r := wait(t, res); !errors.Is(r.err, errRecorderHung) {
		t.Fatalf("got %+v, want errRecorderHung", r)
	}
}

// Lines keep the stall timer off: the run outlasts StallAfter.
func TestRecordTalkingRecorderIsNotStalled(t *testing.T) {
	t.Parallel()
	h := script(t, `for i in 1 2 3 4 5 6 7 8 9 10; do echo '{"event":"segment"}'; sleep 0.7; done`)
	// Longer than the gaps, shorter than the run; the slack covers a slow first exec.
	h.StallAfter = 5 * time.Second
	res, _ := record(h, make(chan struct{}))
	r := wait(t, res)
	if r.err != nil || r.ev.Event != "segment" {
		t.Fatalf("got %+v", r)
	}
}
