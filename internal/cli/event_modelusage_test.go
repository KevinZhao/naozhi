package cli

import "testing"

// resultFrameWithModelUsage is the CLI 2.1.261 result frame captured against
// Bedrock (docs/rfc/cost-ledger.md §1.2), trimmed to the cost fields.
const resultFrameWithModelUsage = `{"type":"result","subtype":"success","is_error":false,"session_id":"s1",` +
	`"result":"ok","total_cost_usd":0.81848,` +
	`"usage":{"input_tokens":2,"cache_creation_input_tokens":40913,"output_tokens":4},` +
	`"modelUsage":{"us.anthropic.claude-fable-5-1[1m]":{"inputTokens":2,"outputTokens":4,` +
	`"cacheReadInputTokens":0,"cacheCreationInputTokens":40913,"webSearchRequests":0,"costUSD":0.81848,` +
	`"contextWindow":1000000,"maxOutputTokens":64000,"thinkingTokens":0,"canonicalModel":"claude-fable-5-1",` +
	`"provider":"bedrock","costBasis":"list"}}}`

func TestReadEvent_ResultCarriesModelUsage(t *testing.T) {
	p := &ClaudeProtocol{}
	evs, done, err := p.ReadEvent(resultFrameWithModelUsage)
	if err != nil || !done || len(evs) != 1 {
		t.Fatalf("ReadEvent err=%v done=%v n=%d", err, done, len(evs))
	}
	ev := evs[0]
	mu, ok := ev.ModelUsage["us.anthropic.claude-fable-5-1[1m]"]
	if !ok {
		t.Fatalf("modelUsage key missing: %+v", ev.ModelUsage)
	}
	if mu.CacheCreationInputTokens != 40913 || mu.OutputTokens != 4 || mu.CostUSD != 0.81848 ||
		mu.CanonicalModel != "claude-fable-5-1" || mu.Provider != "bedrock" || mu.CostBasis != "list" {
		t.Fatalf("modelUsage row = %+v", mu)
	}
	if ev.CostUSD != 0.81848 {
		t.Fatalf("CostUSD = %v", ev.CostUSD)
	}
}

func TestReadEvent_NonResultFrameHasNilModelUsage(t *testing.T) {
	p := &ClaudeProtocol{}
	evs, _, err := p.ReadEvent(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
	if err != nil || len(evs) != 1 {
		t.Fatalf("ReadEvent err=%v n=%d", err, len(evs))
	}
	if evs[0].ModelUsage != nil {
		t.Fatalf("assistant frame must not allocate ModelUsage, got %v", evs[0].ModelUsage)
	}
}

// BenchmarkReadEvent_ResultWithModelUsage records the baseline for the one
// frame per turn that now decodes the per-model map; BenchmarkReadEvent
// (assistant frame) stays the guard that non-result frames pay nothing extra.
func BenchmarkReadEvent_ResultWithModelUsage(b *testing.B) {
	p := &ClaudeProtocol{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := p.ReadEvent(resultFrameWithModelUsage); err != nil {
			b.Fatal(err)
		}
	}
}
