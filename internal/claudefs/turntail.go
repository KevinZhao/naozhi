// turntail.go — whether a session transcript's last turn has ended, read off
// its tail (docs/rfc/workflow-dashboard.md §5.10).
package claudefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/naozhi/naozhi/internal/osutil"
)

// Tail windows TranscriptTurnEnded reads. They bound the read, not the file:
// session transcripts run to many MiB. The wider one is for a last record
// that alone outgrows the first (a large tool_use input).
const (
	turnTailWindow    = 64 << 10
	turnTailMaxWindow = 1 << 20
)

// turnEndStopReasons are the stop reasons after which the CLI waits for the
// next prompt. stop_sequence is what its synthetic API-error messages carry.
var turnEndStopReasons = map[string]bool{"end_turn": true, "stop_sequence": true, "refusal": true}

// TranscriptTurnEnded reports whether the last main-chain record of the
// transcript at path closes a turn: a result, or an assistant message with a
// turn-ending stop reason. A user record, a tool_use stop, or no main-chain
// record within the widest tail window all report false: the turn may still
// be running. Metadata, attachment and system lines are skipped.
func TranscriptTurnEnded(path string) (bool, error) {
	f, fi, err := osutil.OpenRegular(path, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for _, window := range []int64{turnTailWindow, turnTailMaxWindow} {
		ended, found, err := lastTurnRecord(f, fi.Size(), window)
		if err != nil || found {
			return ended, err
		}
		if window >= fi.Size() {
			break
		}
	}
	return false, nil
}

// lastTurnRecord scans the last window bytes of r backwards for the last
// complete main-chain record. A window starting mid-file drops its first,
// partial line.
func lastTurnRecord(r io.ReaderAt, size, window int64) (ended, found bool, err error) {
	off := max(0, size-window)
	buf := make([]byte, size-off)
	n, err := r.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, false, err
	}
	buf = buf[:n]
	if off > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return false, false, nil
		}
		buf = buf[i+1:]
	}
	for len(buf) > 0 {
		i := bytes.LastIndexByte(buf, '\n')
		line := buf[i+1:]
		buf = buf[:max(i, 0)]
		if ended, ok := turnRecord(line); ok {
			return ended, true, nil
		}
	}
	return false, false, nil
}

// turnRecord classifies one transcript line; ok is false for a line that is
// not a main-chain record (including a partial or undecodable one).
func turnRecord(line []byte) (ended, ok bool) {
	var rec struct {
		Type        string          `json:"type"`
		IsSidechain bool            `json:"isSidechain"`
		Message     json.RawMessage `json:"message"`
	}
	if len(line) == 0 || json.Unmarshal(line, &rec) != nil || rec.IsSidechain {
		return false, false
	}
	switch rec.Type {
	case "result":
		return true, true
	case "user":
		return false, true
	case "assistant":
		var msg struct {
			StopReason string `json:"stop_reason"`
		}
		_ = json.Unmarshal(rec.Message, &msg)
		return turnEndStopReasons[msg.StopReason], true
	}
	return false, false
}
