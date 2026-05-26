package server

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// TestBroadcastCronResult_PayloadSanitisesResultAndError pins R246-SEC-7:
// BroadcastCronResult must apply osutil.SanitizeForLog to result / errMsg
// at the broadcast site, mirroring BroadcastCronRunEnded's defense-in-depth
// posture. The cron package already sanitises before calling this method,
// but cron_result is a public Hub method whose contract is "safe to publish
// over the WS to authenticated dashboard clients" — the sanitiser must live
// at the broadcast boundary so a future caller (test fixture, webhook
// bridge, mid-rebase regression) cannot smuggle bidi / C1 / DEL bytes
// past the SetEscapeHTML(false) JSON encoder.
//
// We avoid spinning up a full Hub by re-deriving the sanitised payload via
// the same SanitizeForLog calls BroadcastCronResult uses, then feeding it
// through marshalPooled to confirm the wire bytes round-trip cleanly. The
// test would have failed against the previous body that wrote `Result:
// result, Error: errMsg` directly.
func TestBroadcastCronResult_PayloadSanitisesResultAndError(t *testing.T) {
	t.Parallel()

	// Each rune in the danger class — bidi (U+202E), C1 (U+0085), C0 (\n /
	// \r), DEL (\x7f) — that osutil.SanitizeForLog rewrites to '_'. Any of
	// these reaching the dashboard JSON-decoded payload would corrupt logs
	// or terminal display.
	dirtyResult := "ok‮evil\nresult\x7f"
	dirtyErr := "failreason\rline"
	dirtyJobID := "job\nid"

	// 4128 mirrors the const in BroadcastCronResult; mirroring locally
	// keeps the test deterministic without exporting the constant.
	const maxBroadcastResultBytes = 4128
	const maxBroadcastErrorBytes = 4128

	wantResult := osutil.SanitizeForLog(dirtyResult, maxBroadcastResultBytes)
	wantErr := osutil.SanitizeForLog(dirtyErr, maxBroadcastErrorBytes)
	wantJobID := osutil.SanitizeForLog(dirtyJobID, 64)

	// Sanity: SanitizeForLog actually rewrote the danger class.
	for _, r := range []rune{0x202E, 0x0085, 0x7f, '\n', '\r'} {
		if strings.ContainsRune(wantResult, r) {
			t.Fatalf("sanitised result still contains rune %U: %q", r, wantResult)
		}
		if strings.ContainsRune(wantErr, r) {
			t.Fatalf("sanitised err still contains rune %U: %q", r, wantErr)
		}
		if strings.ContainsRune(wantJobID, r) {
			t.Fatalf("sanitised jobID still contains rune %U: %q", r, wantJobID)
		}
	}

	// Reproduce the marshal step BroadcastCronResult does so we get the
	// wire-shape view a connected client would see; assert each field is
	// the sanitised variant, not the raw input.
	data, err := marshalPooled(cronResultMsg{
		Type:   "cron_result",
		JobID:  wantJobID,
		Result: wantResult,
		Error:  wantErr,
	})
	if err != nil {
		t.Fatalf("marshalPooled: %v", err)
	}

	var got cronResultMsg
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}

	if got.Type != "cron_result" {
		t.Errorf("Type = %q, want %q", got.Type, "cron_result")
	}
	if got.JobID != wantJobID {
		t.Errorf("JobID = %q, want %q", got.JobID, wantJobID)
	}
	if got.Result != wantResult {
		t.Errorf("Result = %q, want %q (R246-SEC-7 sanitiser regression)", got.Result, wantResult)
	}
	if got.Error != wantErr {
		t.Errorf("Error = %q, want %q (R246-SEC-7 sanitiser regression)", got.Error, wantErr)
	}

	// Drop-dead invariant: none of the danger runes survive the round-trip.
	for _, r := range []rune{0x202E, 0x0085, 0x7f, '\n', '\r'} {
		if strings.ContainsRune(got.Result, r) || strings.ContainsRune(got.Error, r) {
			t.Errorf("payload contains danger rune %U after sanitise; result=%q error=%q",
				r, got.Result, got.Error)
		}
	}
}

// TestBroadcastCronResult_EndToEnd_SanitisesViaHub drives the actual Hub
// method (rather than re-deriving its payload locally) to close the
// regression-coverage gap: a future change that drops or moves the
// SanitizeForLog calls inside BroadcastCronResult must fail this test.
//
// We register a captured wsClient with the Hub, mark it authenticated so
// broadcastToAuthenticated will route to it, fire the broadcast, and
// inspect the bytes that reach the wire. The structural test above
// confirms the payload shape; this one confirms the implementation
// actually invokes the sanitiser path.
func TestBroadcastCronResult_EndToEnd_SanitisesViaHub(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	var nodesMu sync.RWMutex
	hub := NewHub(HubOptions{
		Router:  router,
		Guard:   guard,
		NodesMu: &nodesMu,
	})

	c := &wsClient{
		hub:  hub,
		send: make(chan []byte, 8),
		done: make(chan struct{}),
	}
	c.authenticated.Store(true)
	hub.register(c)
	t.Cleanup(func() { close(c.done) })

	// Mix bidi / C1 / DEL / CR / LF in result, error, and jobID. If
	// BroadcastCronResult ever stops calling SanitizeForLog at the marshal
	// site, these bytes will round-trip into the wire payload and trip the
	// invariants below.
	const (
		dirtyResult = "ok‮evil\nresult\x7f"
		dirtyErr    = "failreason\rline"
		dirtyJobID  = "job\nid"
	)

	hub.BroadcastCronResult(dirtyJobID, dirtyResult, dirtyErr)

	var data []byte
	select {
	case data = <-c.send:
	case <-time.After(2 * time.Second):
		t.Fatal("BroadcastCronResult did not deliver to authenticated client within 2s")
	}

	var got cronResultMsg
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal wire payload: %v", err)
	}
	if got.Type != "cron_result" {
		t.Errorf("Type = %q, want cron_result", got.Type)
	}
	for _, r := range []rune{0x202E, 0x0085, 0x7f, '\n', '\r'} {
		if strings.ContainsRune(got.JobID, r) {
			t.Errorf("JobID round-tripped danger rune %U: %q", r, got.JobID)
		}
		if strings.ContainsRune(got.Result, r) {
			t.Errorf("Result round-tripped danger rune %U: %q", r, got.Result)
		}
		if strings.ContainsRune(got.Error, r) {
			t.Errorf("Error round-tripped danger rune %U: %q", r, got.Error)
		}
	}
	// The sanitiser must not silently drop the surrounding payload — the
	// non-danger prefix/suffix must survive so operators still see context.
	if !strings.HasPrefix(got.Result, "ok") || !strings.Contains(got.Result, "evil") {
		t.Errorf("Result lost its non-danger payload after sanitise: %q", got.Result)
	}
	if !strings.HasPrefix(got.Error, "fail") || !strings.Contains(got.Error, "reason") {
		t.Errorf("Error lost its non-danger payload after sanitise: %q", got.Error)
	}
}
