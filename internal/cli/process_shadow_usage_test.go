package cli

import "testing"

// TestShadowUsage_AccumulatesUntilResult pins the shadow account: assistant
// frames' usage sums per turn, the result frame clears it, Take resets it.
func TestShadowUsage_AccumulatesUntilResult(t *testing.T) {
	p := &Process{}
	asst := func(in, out int64, model string) Event {
		return Event{Type: "assistant", Message: &AssistantMessage{Model: model,
			Usage: &MessageUsage{InputTokens: in, OutputTokens: out, CacheCreationInputTokens: 10}}}
	}
	p.trackShadowUsage(asst(5, 7, "us.anthropic.claude-fable-5-1[1m]"))
	p.trackShadowUsage(Event{Type: "assistant", Message: &AssistantMessage{}}) // no usage: ignored
	p.trackShadowUsage(asst(1, 2, ""))
	u := p.TakeShadowUsage()
	if u.Input != 6 || u.Output != 9 || u.CacheWrite != 20 || u.Model != "us.anthropic.claude-fable-5-1[1m]" || u.IsZero() {
		t.Fatalf("shadow = %+v", u)
	}
	if !p.TakeShadowUsage().IsZero() {
		t.Fatal("Take must clear the account")
	}
	p.trackShadowUsage(asst(3, 3, "m"))
	p.trackShadowUsage(Event{Type: "result", CostUSD: 1})
	if !p.TakeShadowUsage().IsZero() {
		t.Fatal("result frame must clear the account (its modelUsage supersedes it)")
	}
}

func TestReadEvent_AssistantUsageParsed(t *testing.T) {
	pr := &ClaudeProtocol{}
	evs, _, err := pr.ReadEvent(`{"type":"assistant","message":{"role":"assistant","model":"claude-fable-5-1",` +
		`"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":5,"cache_creation_input_tokens":6}}}`)
	if err != nil || len(evs) != 1 || evs[0].Message == nil || evs[0].Message.Usage == nil {
		t.Fatalf("ReadEvent err=%v evs=%+v", err, evs)
	}
	u := evs[0].Message.Usage
	if evs[0].Message.Model != "claude-fable-5-1" || u.InputTokens != 3 || u.OutputTokens != 4 || u.CacheReadInputTokens != 5 || u.CacheCreationInputTokens != 6 {
		t.Fatalf("usage = %+v model=%q", u, evs[0].Message.Model)
	}
}
