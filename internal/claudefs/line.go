// line.go — the transcript line schema and its timestamp (#2643).
package claudefs

import (
	"encoding/json"
	"time"
)

// Line is the part of a Claude transcript line naozhi reads, keeping the
// message body deferred. Two packages decoded a subset of this independently
// (discovery's historyLine was a strict subset of dashboard/cron's
// claudeJSONLEvent), so a field the CLI added — `toolUseResult`, whose placement
// "varies by CLI version" — was visible to one and invisible to the other.
//
// internal/cli's subagent reader deliberately does NOT use this type: it decodes
// `message` eagerly into a typed struct because it needs the content blocks on
// every line, while these two consumers pass the raw bytes on. Collapsing all
// three would force one side to change decode strategy to make a count smaller,
// which is a worse trade than having two shapes for two access patterns.
type Line struct {
	Type      string          `json:"type"`
	SubType   string          `json:"subtype"`
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"` // RFC3339 / RFC3339Nano
	UUID      string          `json:"uuid"`
	Message   json.RawMessage `json:"message"`

	// ToolUseResult: tool_result events sometimes appear at top level here
	// instead of inside a content block (varies by CLI version). Both shapes
	// are tolerated by consumers.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

// ParseTimestamp parses a transcript line's timestamp. RFC3339Nano's layout
// accepts an absent fractional second, so it covers plain RFC3339 too; one of
// the four call sites this replaced carried a redundant second attempt with
// time.RFC3339 for that reason.
func ParseTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// TimestampMillis is ParseTimestamp in unix ms, returning 0 for empty or
// unparseable input so callers can use it as a "skip filter" sentinel.
//
// There is deliberately no hand-written parser for the CLI's canonical "…Z"
// shape (#3522): every caller decodes the whole JSON line first, which costs
// microseconds, so a few ns saved here never shows end to end. Adding one needs
// an end-to-end win such as in BenchmarkLoadHistoryTail_vs_LoadHistory.
func TimestampMillis(s string) int64 {
	t, ok := ParseTimestamp(s)
	if !ok {
		return 0
	}
	return t.UnixMilli()
}
