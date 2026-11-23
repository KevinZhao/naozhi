package cli

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/apiwatch"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
)

// The frames CC 2.1.330 writes: a retried request (with and without a
// response), a turn that went through, one that failed on the API and one
// naozhi stopped. Each is booked once under the process's session key; the
// stopped turn is no failure.
func TestProcessAPIWatch(t *testing.T) {
	t.Parallel()
	store := apiwatch.Open("")
	proto := &ClaudeProtocol{}
	p := &Process{eventLog: ring.NewEventLog(32), eventCh: make(chan clievent.Event, 16), killCh: make(chan struct{})}
	p.setAPIWatch(store, "feishu:direct:alice:general", "claude")
	feed := func(line string) {
		t.Helper()
		evs, _, err := proto.ReadEventInto(line, nil)
		if err != nil {
			t.Fatalf("ReadEventInto(%s): %v", line, err)
		}
		for _, ev := range evs {
			p.dispatchProtocolEvent(ev, slog.New(slog.DiscardHandler))
		}
		for len(p.eventCh) > 0 {
			<-p.eventCh
		}
	}
	feed(`{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1200,"error_status":529,"error":"overloaded","uuid":"r1","session_id":"s"}`)
	feed(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":500,"error_status":null,"error":"unknown","no_response":{"waited_ms":30000,"retry_wait_ms":500},"uuid":"r2","session_id":"s"}`)
	feed(`{"type":"result","subtype":"success","is_error":false,"duration_ms":9000,"duration_api_ms":7000,"ttft_ms":2500,"num_turns":1,"result":"done","total_cost_usd":0.1,"uuid":"t1","session_id":"s"}`)
	feed(`{"type":"result","subtype":"success","is_error":true,"api_error_status":400,"duration_ms":1500,"duration_api_ms":1400,"num_turns":1,"result":"API Error: 400 Access to Anthropic models is not allowed from unsupported countries, regions, or territories.","uuid":"t2","session_id":"s"}`)
	p.turn.abortRequested.arm()
	feed(`{"type":"result","subtype":"error_during_execution","is_error":true,"duration_ms":800,"duration_api_ms":300,"num_turns":1,"uuid":"t3","session_id":"s"}`)

	rep := store.Report(apiwatch.Query{Window: time.Hour})
	if s := rep.Summary; s.Retries != 2 || s.Turns != 3 || s.FailedTurns != 1 || s.RetriedTurns != 1 {
		t.Fatalf("summary = %+v", s)
	}
	byUUID := map[string]apiwatch.Entry{}
	for _, e := range rep.Recent {
		byUUID[e.UUID] = e
	}
	if e := byUUID["r1"]; e.Class != apiwatch.ClassOverloaded || e.Attempt != 2 || e.Status != 529 || e.RetryDelayMS != 1200 || e.Key != "feishu:direct:alice:general" || e.Backend != "claude" {
		t.Errorf("overloaded retry = %+v", e)
	}
	if e := byUUID["r2"]; e.Class != apiwatch.ClassFirstByte || e.WaitedMS != 30000 || e.Status != 0 {
		t.Errorf("first-byte retry = %+v", e)
	}
	if e := byUUID["t2"]; e.Class != apiwatch.ClassRegion || e.Status != 400 || e.APIMS != 1400 {
		t.Errorf("failed turn = %+v", e)
	}
	if _, ok := byUUID["t3"]; ok {
		t.Errorf("a stopped turn was booked as a problem")
	}
	if len(rep.Slowest) == 0 || rep.Slowest[0].UUID != "t1" || rep.Slowest[0].TTFTMS != 2500 || rep.Slowest[0].DurationMS != 9000 {
		t.Errorf("slowest = %+v", rep.Slowest)
	}
}

// A process the watchdog kills books one timeout; another death reason none.
func TestProcessAPIWatchWatchdog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason string
		want   int
	}{{DeathReasonNoOutputTimeout, 1}, {DeathReasonTotalTimeout, 1}, {DeathReasonShimEOF, 0}} {
		store := apiwatch.Open("")
		p := &Process{}
		p.setAPIWatch(store, "k", "claude")
		p.setDeathReason(tc.reason)
		p.setDeathReason(tc.reason) // a second death path: first writer wins
		if got := store.Report(apiwatch.Query{Window: time.Hour}).Summary.Timeouts; got != tc.want {
			t.Errorf("%s: timeouts = %d, want %d", tc.reason, got, tc.want)
		}
	}
}

// A result frame without timing (an older CLI, a synthesized one) parses to nil.
func TestParseAPITimingAbsent(t *testing.T) {
	t.Parallel()
	b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "result": "x"})
	if got := parseAPITiming(string(b)); got != nil {
		t.Errorf("parseAPITiming = %+v, want nil", got)
	}
}
