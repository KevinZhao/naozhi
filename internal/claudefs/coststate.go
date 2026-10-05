// coststate.go — the CLI's persisted running cost, which `claude --resume`
// restores (#3096).
package claudefs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// CostState is a transcript's `"type":"cost-state"` line: the CLI's running
// total for the session, written to the JSONL from time to time. On
// `--resume` the CLI restores the last one, so the first result of the
// resumed process reports this total plus the new turn — not the new turn
// alone. A process that is killed writes none, so several resumes in a row
// can restore the same, older line.
type CostState struct {
	SessionID    string  `json:"sessionId"`
	TotalCostUSD float64 `json:"totalCostUSD"`
	// ModelUsage keeps the per-model rows undecoded: their shape is the
	// result frame's modelUsage, which this leaf package does not import.
	ModelUsage json.RawMessage `json:"modelUsage"`
}

// costStateMarker pre-filters lines before any JSON decode; the decode then
// confirms the type, so a message that merely mentions the word is skipped.
var costStateMarker = []byte(`"cost-state"`)

// maxCostStateLine bounds the lines decoded: a cost-state line is a few KiB,
// while transcripts carry multi-megabyte tool results that are skipped
// unread.
const maxCostStateLine = 1 << 20

// LastCostState returns the last cost-state line of the transcript at path
// that belongs to sessionID (a line without a sessionId is accepted). found
// is false when the transcript has none, which is also what the CLI restores
// then: nothing. A missing file is an error, not "none": the caller asked
// about a transcript it is about to resume.
func LastCostState(path, sessionID string) (st CostState, found bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return CostState{}, false, err
	}
	defer f.Close()
	err = eachLine(f, maxCostStateLine, func(line []byte) {
		if s, ok := decodeCostState(line, sessionID); ok {
			st, found = s, true
		}
	})
	if err != nil {
		return CostState{}, false, err
	}
	return st, found, nil
}

// CostStateMark is a cost-state line placed in time. The line carries no
// timestamp of its own, so it is bracketed by its neighbours: Before is the
// last timestamp ahead of it, After the first one past it (zero when no
// timestamped line follows).
type CostStateMark struct {
	CostState
	Before, After time.Time
}

// timestampMarker pre-filters the lines whose timestamp CostStates decodes.
var timestampMarker = []byte(`"timestamp"`)

// CostStates lists the transcript's cost-state lines that belong to
// sessionID (or name none), in file order.
func CostStates(path, sessionID string) ([]CostStateMark, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []CostStateMark
	var last time.Time
	open := 0 // out[open:] still wait for their After
	err = eachLine(f, maxCostStateLine, func(line []byte) {
		if s, ok := decodeCostState(line, sessionID); ok {
			out = append(out, CostStateMark{CostState: s, Before: last})
			return
		}
		if !bytes.Contains(line, timestampMarker) {
			return
		}
		var v struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(line, &v) != nil {
			return
		}
		ms := TimestampMillis(v.Timestamp)
		if ms == 0 {
			return
		}
		last = time.UnixMilli(ms).UTC()
		for ; open < len(out); open++ {
			out[open].After = last
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// eachLine calls fn with every line of r up to maxLine bytes, newline
// included; longer lines are skipped unread. A line longer than the reader's
// buffer arrives in pieces: it is gathered while it could still fit and
// skipped once it cannot.
func eachLine(r io.Reader, maxLine int, fn func(line []byte)) error {
	return eachLineAt(r, maxLine, func(_ int64, line []byte) bool {
		fn(line)
		return true
	})
}

// eachLineAt is eachLine passing each line's offset in r; fn returning false
// ends the read.
func eachLineAt(r io.Reader, maxLine int, fn func(off int64, line []byte) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var long []byte
	skipping := false
	var off, next int64
	for {
		chunk, rerr := br.ReadSlice('\n')
		next += int64(len(chunk))
		if errors.Is(rerr, bufio.ErrBufferFull) {
			if !skipping {
				long = append(long, chunk...)
				if len(long) > maxLine {
					skipping, long = true, long[:0]
				}
			}
			continue
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return rerr
		}
		if skipping {
			skipping = false
		} else {
			if len(long) > 0 {
				chunk = append(long, chunk...)
			}
			if !fn(off, chunk) {
				return nil
			}
		}
		long = long[:0]
		off = next
		if rerr != nil {
			return nil
		}
	}
}

func decodeCostState(line []byte, sessionID string) (CostState, bool) {
	if !bytes.Contains(line, costStateMarker) {
		return CostState{}, false
	}
	var v struct {
		Type string `json:"type"`
		CostState
	}
	if json.Unmarshal(line, &v) != nil || v.Type != "cost-state" {
		return CostState{}, false
	}
	if v.SessionID != "" && v.SessionID != sessionID {
		return CostState{}, false
	}
	return v.CostState, true
}
