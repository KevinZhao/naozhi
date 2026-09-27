package cli

import (
	"testing"
)

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
