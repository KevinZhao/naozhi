package cli

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

func TestReadEvent_AssistantUsageParsed(t *testing.T) {
	pr := &ClaudeProtocol{}
	evs, _, err := pr.ReadEvent(`{"type":"assistant","message":{"id":"msg_bdrk_01","role":"assistant","model":"claude-fable-5-1",` +
		`"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":5,"cache_creation_input_tokens":6}}}`)
	if err != nil || len(evs) != 1 || evs[0].Message == nil || evs[0].Message.Usage == nil {
		t.Fatalf("ReadEvent err=%v evs=%+v", err, evs)
	}
	u := evs[0].Message.Usage
	if evs[0].Message.ID != "msg_bdrk_01" || evs[0].Message.Model != "claude-fable-5-1" || u.InputTokens != 3 || u.OutputTokens != 4 || u.CacheReadInputTokens != 5 || u.CacheCreationInputTokens != 6 {
		t.Fatalf("usage = %+v model=%q", u, evs[0].Message.Model)
	}
}

// TestProcess_ShadowThroughReadLoop: the read loop feeds every frame to the
// shadow account, so the per-block frames of one API message count once, and
// a result frame stamps LastResultAt with its receive time.
func TestProcess_ShadowThroughReadLoop(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	defer p.Kill()
	p.startReadLoop()

	frame := func(text string, out int) string {
		return `{"type":"assistant","message":{"id":"msg_1","role":"assistant","model":"opus",` +
			`"content":[{"type":"text","text":"` + text + `"}],"usage":{"input_tokens":3,"output_tokens":` +
			strconv.Itoa(out) + `,"cache_creation_input_tokens":21584}}}`
	}
	srv.SendStdout(frame("a", 8))
	srv.SendStdout(frame("b", 224))
	testhelper.Eventually(t, func() bool { return len(p.eventLog.EntriesSince(0)) >= 2 }, 2*time.Second, "assistant frames not logged")
	want := []clievent.ShadowModel{{Model: "opus", Input: 3, Output: 224, CacheWrite: 21584}}
	if got := p.meter.TakeShadow().Models; !reflect.DeepEqual(got, want) {
		t.Fatalf("shadow = %+v, want %+v", got, want)
	}

	if !p.LastResultAt().IsZero() {
		t.Fatal("LastResultAt before any result must be zero")
	}
	before := time.Now().Truncate(time.Millisecond)
	srv.SendStdout(`{"type":"result","subtype":"success","result":"ok","session_id":"s1","total_cost_usd":0.25}`)
	testhelper.Eventually(t, func() bool { return !p.LastResultAt().IsZero() }, 2*time.Second, "result not stamped")
	if at := p.LastResultAt(); at.Before(before) || at.After(time.Now()) {
		t.Fatalf("LastResultAt = %v, want within [%v, now]", at, before)
	}
}
