// usage.go — the token use a session's transcripts record (#3210).
package claudefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ModelTokens is one model's token use; Model is the raw id the transcript
// lines name.
type ModelTokens struct {
	Model                                string
	Input, Output, CacheRead, CacheWrite int64
}

// UsageWindow selects transcript lines by timestamp: after Since, up to and
// including Until (zero: no upper bound). MainOffset is where reading the
// main transcript starts, a size it had before the window began; a line the
// offset cuts through fails to decode and is skipped. A file with more than
// seekPast bytes to read may start further on (usageStart).
type UsageWindow struct {
	Since, Until time.Time
	MainOffset   int64
}

// maxUsageLine bounds the lines decoded. An assistant line carries one
// content block, so only a block of megabytes (a huge tool input) is skipped,
// and the other lines of its message repeat the usage.
const maxUsageLine = 1 << 20

// maxUsageFiles bounds the sub-agent transcripts one call opens.
const maxUsageFiles = 512

// A windowed read with more than seekPast bytes ahead bisects for its window,
// stopping seekSlack before Since; a probe reads at most maxUsageProbe bytes
// looking for an assistant usage line.
const (
	seekPast      = 8 << 20
	seekSlack     = time.Minute
	maxUsageProbe = 4 * maxUsageLine
)

var (
	assistantMarker = []byte(`"assistant"`)
	usageMarker     = []byte(`"usage"`)
)

// SessionUsage sums the usage the assistant lines of a session's transcripts
// record in w: the main transcript plus every sub-agent's and hosted-workflow
// agent's (SubagentsDir/agent-*.jsonl, SubagentsDir/workflows/*/agent-*.jsonl).
// The lines of one API message repeat its usage, so each message counts once,
// at the largest value each field reached. Sub-agent files not modified since
// w.Since are not opened. found is false when the main transcript does not
// exist; rows come in first-seen model order.
func SessionUsage(projectDir, sessionID string, w UsageWindow) (usage []ModelTokens, found bool, err error) {
	acc := newUsageAcc(w)
	found, err = readSessionUsage(projectDir, sessionID, w, acc)
	if err != nil || !found {
		return nil, found, err
	}
	return acc.totals(), true, nil
}

// readSessionUsage feeds the session's main transcript and its agent
// transcripts to acc.
func readSessionUsage(projectDir, sessionID string, w UsageWindow, acc *usageAcc) (found bool, err error) {
	if projectDir == "" || !IsValidSessionID(sessionID) {
		return false, nil
	}
	f, err := os.Open(TranscriptIn(projectDir, sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = readUsageFrom(f, w.MainOffset, w.Since, acc)
	f.Close()
	if err != nil {
		return true, err
	}
	paths := agentTranscripts(SubagentsDir(projectDir, sessionID), w.Since)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue // removed meanwhile, or unreadable: its usage is lost, the rest still counts
		}
		err = readUsageFrom(f, 0, w.Since, acc)
		f.Close()
		if err != nil {
			return true, err
		}
	}
	return true, nil
}

// readUsageFrom feeds f's lines from usageStart on to acc.
func readUsageFrom(f *os.File, offset int64, since time.Time, acc *usageAcc) error {
	if _, err := f.Seek(usageStart(f, offset, since), io.SeekStart); err != nil {
		return err
	}
	return eachLine(f, maxUsageLine, acc.line)
}

// usageStart is where reading f for the lines after since begins: at offset,
// or the start when offset is past the end (the file was replaced). With more
// than seekPast bytes ahead, usageSeek moves it on to seekSlack before since.
func usageStart(f *os.File, offset int64, since time.Time) int64 {
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	if offset > st.Size() {
		offset = 0
	}
	if since.IsZero() || st.Size()-offset <= seekPast {
		return offset
	}
	return usageSeek(f, offset, st.Size(), since.Add(-seekSlack).UnixMilli())
}

// usageSeek bisects f's bytes from lo to hi for the start of an assistant
// usage line at or before cutoff (unix ms), within maxUsageLine of the last
// such line. The CLI appends those lines in time order, so none before it is
// later than cutoff. A probe finding no usage line counts as later, which only
// moves the start earlier.
func usageSeek(f *os.File, lo, hi, cutoff int64) int64 {
	for hi-lo > maxUsageLine {
		mid := lo + (hi-lo)/2
		if at, ms, ok := probeUsage(f, mid); ok && ms <= cutoff {
			lo = at
		} else {
			hi = mid
		}
	}
	return lo
}

// probeUsage finds the first assistant usage line starting at or after from,
// within maxUsageProbe bytes: its offset and timestamp. The line from cuts
// through fails to decode, as at MainOffset.
func probeUsage(f *os.File, from int64) (at, ms int64, ok bool) {
	r := io.NewSectionReader(f, from, maxUsageProbe)
	_ = eachLineAt(r, maxUsageLine, func(off int64, b []byte) bool {
		row, _, decoded := decodeUsage(b)
		if decoded {
			at, ms, ok = from+off, row.ms, true
		}
		return !decoded
	})
	return at, ms, ok
}

// agentTranscripts lists the sub-agent and workflow-agent transcripts under
// dir modified at or after since, at most maxUsageFiles of them. Only regular files count, so a symlink planted
// in the tree is never followed.
func agentTranscripts(dir string, since time.Time) (out []string) {
	if dir == "" {
		return nil
	}
	add := func(d string) {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			name := e.Name()
			if !e.Type().IsRegular() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			if info, err := e.Info(); err != nil || info.ModTime().Before(since) {
				continue
			}
			if len(out) >= maxUsageFiles {
				return
			}
			out = append(out, filepath.Join(d, name))
		}
	}
	add(dir)
	wfs, _ := os.ReadDir(WorkflowRunsDir(dir))
	for _, wf := range wfs {
		if wf.IsDir() {
			add(filepath.Join(WorkflowRunsDir(dir), wf.Name()))
		}
	}
	return out
}

// usageAcc keeps the usage last seen per API message in the window.
type usageAcc struct {
	sinceMS, untilMS int64
	msgs             map[string]int // message id -> index in rows
	rows             []usageRow
}

// usageRow is one message's usage and the time of its first line.
type usageRow struct {
	ModelTokens
	ms int64
}

func newUsageAcc(w UsageWindow) *usageAcc {
	a := &usageAcc{sinceMS: w.Since.UnixMilli(), untilMS: w.Until.UnixMilli(), msgs: make(map[string]int)}
	if w.Until.IsZero() {
		a.untilMS = 0
	}
	return a
}

// line folds one transcript line in, if it is an assistant line in the window.
func (a *usageAcc) line(b []byte) {
	row, id, ok := decodeUsage(b)
	if !ok || row.ms <= a.sinceMS || (a.untilMS != 0 && row.ms > a.untilMS) {
		return
	}
	if i, ok := a.msgs[id]; ok && id != "" {
		r := &a.rows[i]
		r.Input, r.Output = max(r.Input, row.Input), max(r.Output, row.Output)
		r.CacheRead, r.CacheWrite = max(r.CacheRead, row.CacheRead), max(r.CacheWrite, row.CacheWrite)
		return
	}
	if id != "" {
		a.msgs[id] = len(a.rows)
	}
	a.rows = append(a.rows, row)
}

// decodeUsage decodes b if it is a timestamped assistant line with usage:
// the line's usage and time, and its message id.
func decodeUsage(b []byte) (row usageRow, id string, ok bool) {
	if !bytes.Contains(b, assistantMarker) || !bytes.Contains(b, usageMarker) {
		return usageRow{}, "", false
	}
	var v struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				Output     int64 `json:"output_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &v) != nil || v.Type != "assistant" || v.Message.Usage == nil {
		return usageRow{}, "", false
	}
	ts := TimestampMillis(v.Timestamp)
	if ts == 0 {
		return usageRow{}, "", false
	}
	u := v.Message.Usage
	t := ModelTokens{Model: v.Message.Model, Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
	return usageRow{ModelTokens: t, ms: ts}, v.Message.ID, true
}

// totals sums the messages per model.
func (a *usageAcc) totals() []ModelTokens {
	var out []ModelTokens
	idx := make(map[string]int)
	for _, r := range a.rows {
		out = addTokens(out, idx, r.ModelTokens)
	}
	return out
}

// addTokens folds t into its model's row of out (idx: model -> index);
// a message with no tokens adds nothing.
func addTokens(out []ModelTokens, idx map[string]int, t ModelTokens) []ModelTokens {
	if t.Input == 0 && t.Output == 0 && t.CacheRead == 0 && t.CacheWrite == 0 {
		return out
	}
	i, ok := idx[t.Model]
	if !ok {
		i = len(out)
		idx[t.Model] = i
		out = append(out, ModelTokens{Model: t.Model})
	}
	o := &out[i]
	o.Input += t.Input
	o.Output += t.Output
	o.CacheRead += t.CacheRead
	o.CacheWrite += t.CacheWrite
	return out
}
