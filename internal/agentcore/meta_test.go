package agentcore

import (
	"context"
	"encoding/json"
	"testing"
)

func TestResultMetaOf(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		wantCost float64
		wantDur  int64
	}{
		{
			name:     "result event carries cost + duration",
			line:     `{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.0044,"duration_ms":1888}`,
			wantOK:   true,
			wantCost: 0.0044,
			wantDur:  1888,
		},
		{
			name:   "non-result line yields nothing",
			line:   `{"type":"assistant","message":{}}`,
			wantOK: false,
		},
		{
			name:   "marker-gated: a tool result mentioning the word result must not match",
			line:   `{"type":"user","message":{"content":"the result is 42"}}`,
			wantOK: false,
		},
		{
			name:     "result with missing cost decodes zero",
			line:     `{"type":"result","is_error":false}`,
			wantOK:   true,
			wantCost: 0,
			wantDur:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := ResultMetaOf(json.RawMessage(tt.line))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if m.CostUSD != tt.wantCost {
					t.Errorf("cost = %v, want %v", m.CostUSD, tt.wantCost)
				}
				if m.DurationMS != tt.wantDur {
					t.Errorf("duration = %v, want %v", m.DurationMS, tt.wantDur)
				}
			}
		})
	}
}

// TestRun_CapturesMetaFromStream pins the end-to-end agentcore capture:
// cost/duration from the result event, image/memory from the meta frame.
func TestRun_CapturesMetaFromStream(t *testing.T) {
	api := &fakeAPI{invokeBody: sse(
		`{"kind":"cli","line":{"type":"system","subtype":"init"},"ts":"t"}`,
		`{"kind":"cli","line":{"type":"result","is_error":false,"total_cost_usd":0.019,"duration_ms":5012},"ts":"t"}`,
		`{"kind":"meta","image_version":"phase2","memory_peak_bytes":268435456,"ts":"t"}`,
		`{"kind":"exit","code":0,"ts":"t"}`,
	)}
	c := newTestClient(api)
	res, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "x"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != Success {
		t.Fatalf("state = %q, want success", res.State)
	}
	if res.CostUSD != 0.019 {
		t.Errorf("cost = %v, want 0.019", res.CostUSD)
	}
	if res.DurationMS != 5012 {
		t.Errorf("duration = %v, want 5012", res.DurationMS)
	}
	if res.ImageVersion != "phase2" {
		t.Errorf("image = %q, want phase2", res.ImageVersion)
	}
	if res.MemoryPeakBytes != 268435456 {
		t.Errorf("memory = %v, want 268435456", res.MemoryPeakBytes)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %v, want 0", res.ExitCode)
	}
}

// TestRun_MetaFrameFilteredFromSink: the meta frame is an execution receipt,
// not a job event — it must not reach the sink (which feeds the event log /
// dashboard message render).
func TestRun_MetaFrameReachesSink(t *testing.T) {
	// Unlike keepalive, the meta frame is NOT filtered: it is a legitimate
	// terminal-area event the dashboard event log may show. Assert it flows
	// to the sink so the contract is explicit (change-detector).
	api := &fakeAPI{invokeBody: sse(
		`{"kind":"meta","image_version":"phase2","ts":"t"}`,
		`{"kind":"cli","line":{"type":"result","is_error":false},"ts":"t"}`,
		`{"kind":"exit","code":0,"ts":"t"}`,
	)}
	c := newTestClient(api)
	var kinds []EnvelopeKind
	_, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "x"},
		func(env *Envelope) error { kinds = append(kinds, env.Kind); return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawMeta bool
	for _, k := range kinds {
		if k == KindMeta {
			sawMeta = true
		}
	}
	if !sawMeta {
		t.Fatalf("meta frame must reach the sink; saw kinds %v", kinds)
	}
}
