package cli

// peer_frames.go — the frames ClaudeProtocol decodes in a second pass because
// their keys clash with, or are absent from, clievent.Event: an inbound
// cross-session message, a held one, and the two refusal frames
// (docs/rfc/planner.md §3.4, §6.2).

import (
	"cmp"
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// peerOriginGate is what a user frame delivering a cross-session message
// contains; every other user frame skips the second decode. The CLI writes
// compact JSON.
const peerOriginGate = `"kind":"peer"`

// wirePeerOrigin is the origin CC stamps on a cross-session message's user
// frame (a replay under --replay-user-messages, idle or mid-turn alike). The
// wrapped <cross-session-message> text in message.content is the model's
// copy; origin carries the same fields unwrapped.
type wirePeerOrigin struct {
	Origin *struct {
		Kind     string `json:"kind"`
		Name     string `json:"name"`
		FromMode string `json:"fromMode"`
		Body     string `json:"body"`
	} `json:"origin"`
}

// parsePeerOrigin returns the inbound message a user frame delivers, nil
// when the frame did not come from another session.
func parsePeerOrigin(line string) *clievent.Peer {
	var w wirePeerOrigin
	if json.Unmarshal(stringToBytesUnsafe(line), &w) != nil || w.Origin == nil || w.Origin.Kind != "peer" {
		return nil
	}
	return &clievent.Peer{Name: w.Origin.Name, Mode: w.Origin.FromMode, Body: w.Origin.Body}
}

// wireSystemDetail holds the system-frame fields Event does not: those of
// peer_message_hold, permission_denied and model_refusal_no_fallback.
// permission_denied's "message" is a string, which Event decodes as an
// assistant message; decodeClaudeEvent tolerates that clash.
type wireSystemDetail struct {
	FromName           string `json:"from_name"`
	State              string `json:"state"`
	Cause              string `json:"cause"`
	Outcome            string `json:"outcome"`
	ToolName           string `json:"tool_name"`
	DecisionReason     string `json:"decision_reason"`
	Message            string `json:"message"`
	Content            string `json:"content"`
	RefusalCategory    string `json:"api_refusal_category"`
	RefusalExplanation string `json:"api_refusal_explanation"`
}

// parseSystemDetail fills ev.Peer or ev.Refusal for the system subtypes that
// carry one; any other frame is left alone.
func parseSystemDetail(line string, ev *clievent.Event) {
	switch ev.SubType {
	case "peer_message_hold", "permission_denied", "model_refusal_no_fallback":
	default:
		return
	}
	var w wireSystemDetail
	if json.Unmarshal(stringToBytesUnsafe(line), &w) != nil {
		return
	}
	switch ev.SubType {
	case "peer_message_hold":
		ev.Peer = &clievent.Peer{Name: w.FromName, State: w.State, Cause: w.Cause, Outcome: w.Outcome}
	case "permission_denied":
		ev.Refusal = &clievent.Refusal{Tool: w.ToolName, Reason: w.DecisionReason, Text: w.Message}
	case "model_refusal_no_fallback":
		ev.Refusal = &clievent.Refusal{Reason: w.RefusalCategory, Text: cmp.Or(w.Content, w.RefusalExplanation)}
	}
}

// eventMessageShadowed decodes an Event whose "message" is not an assistant
// message (permission_denied puts its rejection text there): the outer field
// dominates the embedded one of the same key.
type eventMessageShadowed struct {
	clievent.Event
	Message json.RawMessage `json:"message"`
}

// decodeSystemMessageClash retries a failed decode with "message" skipped and
// keeps the result only for a system frame, the one kind whose "message" may
// be a string; any other frame returns the original err.
func decodeSystemMessageClash(data []byte, ev *clievent.Event, err error) error {
	var shadow eventMessageShadowed
	if json.Unmarshal(data, &shadow) != nil || shadow.Type != "system" {
		return err
	}
	*ev = shadow.Event
	return nil
}
