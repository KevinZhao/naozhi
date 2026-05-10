package schema

import (
	"encoding/json"
	"errors"
	"fmt"
)

// EntryView is the minimal, stable read-side contract the schema
// package uses to introspect an "entry" payload without importing
// the cli package.
//
// cli.EventEntry satisfies this interface (via the adapter below),
// but tests and alternate backends can also provide their own
// implementation — the point is that schema round-trip tests can
// assert "these fields survived" without knowing the full cli type.
//
// Any field NEW to cli.EventEntry that is load-bearing for the
// persistence / merge layer SHOULD appear on EntryView so schema
// tests catch its absence during CI rather than the first production
// read. Today's minimum set is:
//
//   - Time: unix ms; drives the primary sort key for MergedSource
//   - UUID: opaque identity for exact-match dedup between local and
//     Claude JSONL sources (see RFC §3.5.2)
//   - Kind: EventEntry.Type (e.g. "user", "text", "tool_use") —
//     schema uses it to decide rotate / stats sampling
//   - HasImages: whether Images is non-empty, used by the rotate
//     heuristic to bias keeping image-carrying entries
type EntryView interface {
	Time() int64
	UUID() string
	Kind() string
	HasImages() bool
}

// DecodeEntryView parses a json.RawMessage payload into a read-only
// view. The returned view reflects ONLY the EntryView surface; extra
// fields remain in the raw bytes (Record.Entry is still authoritative
// for downstream consumers).
//
// This is the function schema-level tests use to validate that
// cli.EventEntry's JSON shape continues to carry the required fields:
// a fixture EventEntry → MarshalRecord → UnmarshalRecord →
// DecodeEntryView round-trip must preserve every EntryView accessor.
func DecodeEntryView(entry json.RawMessage) (EntryView, error) {
	if len(entry) == 0 {
		return nil, ErrEntryMissingPayload
	}
	var shape minimalEntry
	if err := json.Unmarshal(entry, &shape); err != nil {
		return nil, fmt.Errorf("decode entry view: %w", err)
	}
	return shape, nil
}

// minimalEntry is the private concrete EntryView implementation.
// Field tags intentionally match the persisted cli.EventEntry shape
// (see cli/eventlog.go:EventEntry godoc). Changes here MUST be
// mirrored in cli.EventEntry's tags, otherwise the adapter test
// (entryview_test.go TestEntryViewRoundTrip) will catch the drift.
type minimalEntry struct {
	TimeMS    int64    `json:"time"`
	UUIDField string   `json:"uuid"`
	TypeField string   `json:"type"`
	Images    []string `json:"images,omitempty"`
}

func (m minimalEntry) Time() int64  { return m.TimeMS }
func (m minimalEntry) UUID() string { return m.UUIDField }
func (m minimalEntry) Kind() string { return m.TypeField }
func (m minimalEntry) HasImages() bool {
	return len(m.Images) > 0
}

// ErrEntryPayloadInvalid is returned when DecodeEntryView receives
// bytes that don't deserialize into the EntryView shape. Unlike
// ErrUnsupportedVersion, this is a per-record problem; callers
// should skip the record and continue reading.
var ErrEntryPayloadInvalid = errors.New("schema: entry payload invalid")
