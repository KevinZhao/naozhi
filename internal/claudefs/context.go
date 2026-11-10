// context.go — how big a session's context was at its last API call, read
// off its transcript's tail (docs/rfc/planner.md §15.12 ⑧, the Planner's
// change of shift by size).
package claudefs

import (
	"bytes"
	"encoding/json"

	"github.com/naozhi/naozhi/internal/osutil"
)

// LastContextTokens is the context the last main-chain assistant line of
// the transcript at path was sent with: input + cache_read + cache_creation
// of its usage, what the next call starts from. found is false when no such
// line is within the widest tail window (a fresh transcript, or one whose
// last record outgrows it).
func LastContextTokens(path string) (tokens int64, found bool, err error) {
	f, fi, err := osutil.OpenRegular(path, 0)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	for _, window := range []int64{turnTailWindow, turnTailMaxWindow} {
		buf, err := readTail(f, fi.Size(), window)
		if err != nil {
			return 0, false, err
		}
		for len(buf) > 0 {
			i := bytes.LastIndexByte(buf, '\n')
			line := buf[i+1:]
			buf = buf[:max(i, 0)]
			if n, ok := contextOf(line); ok {
				return n, true, nil
			}
		}
		if window >= fi.Size() {
			break
		}
	}
	return 0, false, nil
}

// contextOf is a main-chain assistant line's context size.
func contextOf(line []byte) (int64, bool) {
	if !bytes.Contains(line, assistantMarker) || !bytes.Contains(line, usageMarker) {
		return 0, false
	}
	var v struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &v) != nil || v.Type != "assistant" || v.IsSidechain || v.Message.Usage == nil {
		return 0, false
	}
	u := v.Message.Usage
	return u.Input + u.CacheRead + u.CacheWrite, true
}
