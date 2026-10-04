package cli

import (
	"strconv"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

func toolUseEv(id, name string) clievent.Event {
	return clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{
		Content: []clievent.ContentBlock{{Type: "tool_use", ID: id, Name: name}},
	}}
}

func toolResultEv(id string) clievent.Event {
	return clievent.Event{Type: "user", Message: &clievent.AssistantMessage{
		Content: []clievent.ContentBlock{{Type: "tool_result", ToolUseID: id}},
	}}
}

func acpToolEv(subType, id, title, status string) clievent.Event {
	ev := clievent.Event{Type: "assistant", SubType: subType, ToolUseID: id,
		ToolCall: &clievent.ToolCall{ID: id, Title: title, Status: status}}
	if subType == "tool_use" {
		ev.Message = &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "tool_use", Name: title}}}
	}
	return ev
}

func TestInflightTools_Observe(t *testing.T) {
	t0 := time.Unix(1000, 0)
	heartbeat := clievent.Event{Type: "tool_progress", ToolUseID: "a-heartbeat-0", ToolName: "Bash", ParentToolUseID: "a"}
	cases := []struct {
		name     string
		events   []clievent.Event
		wantTool string // "" = nothing in flight
	}{
		{"claude tool in flight", []clievent.Event{toolUseEv("a", "Bash")}, "Bash"},
		{"claude tool finished", []clievent.Event{toolUseEv("a", "Bash"), toolResultEv("a")}, ""},
		{"heartbeat folds into its tool", []clievent.Event{toolUseEv("a", "Bash"), heartbeat, toolResultEv("a")}, ""},
		{"heartbeat alone opens the tool", []clievent.Event{heartbeat}, "Bash"},
		{"parallel tools: oldest unfinished is reported",
			[]clievent.Event{toolUseEv("a", "Read"), toolUseEv("b", "Bash"), toolResultEv("a")}, "Bash"},
		{"parallel tools: oldest first", []clievent.Event{toolUseEv("a", "Read"), toolUseEv("b", "Bash")}, "Read"},
		{"result clears the turn", []clievent.Event{toolUseEv("a", "Bash"), {Type: "result"}}, ""},
		{"text-only assistant opens nothing", []clievent.Event{{Type: "assistant", Message: &clievent.AssistantMessage{
			Content: []clievent.ContentBlock{{Type: "text", Text: "hi"}}}}}, ""},
		{"id-less tool_use is not tracked", []clievent.Event{toolUseEv("", "Bash")}, ""},
		{"acp tool in progress", []clievent.Event{
			acpToolEv("tool_use", "c1", "go test ./...", ""), acpToolEv("tool_result", "c1", "go test ./...", "in_progress")}, "go test ./..."},
		{"acp tool completed", []clievent.Event{
			acpToolEv("tool_use", "c1", "go test ./...", ""), acpToolEv("tool_result", "c1", "", "completed")}, ""},
		{"acp tool failed", []clievent.Event{
			acpToolEv("tool_use", "c1", "rm", "pending"), acpToolEv("tool_result", "c1", "", "failed")}, ""},
		{"codex item started then completed", []clievent.Event{
			acpToolEv("tool_use", "i1", "commandExecution", "inProgress"), acpToolEv("tool_result", "i1", "", "completed")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr inflightTools
			for i, ev := range tc.events {
				tr.observe(ev, t0.Add(time.Duration(i)*time.Second))
			}
			got, ok := tr.oldest()
			if tc.wantTool == "" {
				if ok {
					t.Errorf("oldest() = %+v, want nothing in flight", got)
				}
				return
			}
			if !ok || got.name != tc.wantTool {
				t.Errorf("oldest() = %+v, %v; want %q", got, ok, tc.wantTool)
			}
		})
	}
}

// A heartbeat for a tracked tool keeps the tool's start time: ToolElapsed is
// how long the tool has run, not how long since its last beat.
func TestInflightTools_HeartbeatKeepsStartTime(t *testing.T) {
	var tr inflightTools
	t0 := time.Unix(1000, 0)
	tr.observe(toolUseEv("a", "Bash"), t0)
	tr.observe(clievent.Event{Type: "tool_progress", ToolUseID: "a-heartbeat-0", ToolName: "Bash", ParentToolUseID: "a"}, t0.Add(30*time.Second))
	if got, _ := tr.oldest(); !got.started.Equal(t0) {
		t.Errorf("started = %v after a heartbeat, want the tool_use time %v", got.started, t0)
	}
}

func TestInflightTools_Bounded(t *testing.T) {
	var tr inflightTools
	for i := range maxInflightTools + 10 {
		tr.observe(toolUseEv("t"+strconv.Itoa(i), "Read"), time.Unix(int64(i), 0))
	}
	if n := len(tr.tools); n != maxInflightTools {
		t.Errorf("tracked %d tools, want the %d cap", n, maxInflightTools)
	}
}
