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
// offset cuts through fails to decode and is skipped.
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
	if projectDir == "" || !IsValidSessionID(sessionID) {
		return nil, false, nil
	}
	acc := newUsageAcc(w)
	f, err := os.Open(TranscriptIn(projectDir, sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	err = readUsageFrom(f, w.MainOffset, acc)
	f.Close()
	if err != nil {
		return nil, true, err
	}
	for _, p := range agentTranscripts(SubagentsDir(projectDir, sessionID), w.Since) {
		f, err := os.Open(p)
		if err != nil {
			continue // removed meanwhile, or unreadable: its usage is lost, the rest still counts
		}
		err = readUsageFrom(f, 0, acc)
		f.Close()
		if err != nil {
			return nil, true, err
		}
	}
	return acc.totals(), true, nil
}

// readUsageFrom feeds f's lines from offset on to acc; an offset past the end
// (the file was replaced) reads from the start.
func readUsageFrom(f *os.File, offset int64, acc *usageAcc) error {
	if offset > 0 {
		if st, err := f.Stat(); err != nil || offset > st.Size() {
			offset = 0
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
	}
	return eachLine(f, maxUsageLine, acc.line)
}

// agentTranscripts lists the sub-agent and workflow-agent transcripts under
// dir modified at or after since, at most maxUsageFiles of them. Only regular
// files count, so a symlink planted in the tree is never followed.
func agentTranscripts(dir string, since time.Time) []string {
	if dir == "" {
		return nil
	}
	var out []string
	add := func(d string) {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			name := e.Name()
			if len(out) >= maxUsageFiles || !e.Type().IsRegular() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			if info, err := e.Info(); err == nil && !info.ModTime().Before(since) {
				out = append(out, filepath.Join(d, name))
			}
		}
	}
	add(dir)
	wfs, _ := os.ReadDir(filepath.Join(dir, "workflows"))
	for _, wf := range wfs {
		if wf.IsDir() {
			add(filepath.Join(dir, "workflows", wf.Name()))
		}
	}
	return out
}

// usageAcc keeps the usage last seen per API message in the window.
type usageAcc struct {
	sinceMS, untilMS int64
	msgs             map[string]int // message id -> index in rows
	rows             []ModelTokens
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
	if !bytes.Contains(b, assistantMarker) || !bytes.Contains(b, usageMarker) {
		return
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
		return
	}
	ts := TimestampMillis(v.Timestamp)
	if ts == 0 || ts <= a.sinceMS || (a.untilMS != 0 && ts > a.untilMS) {
		return
	}
	u := v.Message.Usage
	t := ModelTokens{Model: v.Message.Model, Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
	if i, ok := a.msgs[v.Message.ID]; ok && v.Message.ID != "" {
		r := &a.rows[i]
		r.Input, r.Output = max(r.Input, t.Input), max(r.Output, t.Output)
		r.CacheRead, r.CacheWrite = max(r.CacheRead, t.CacheRead), max(r.CacheWrite, t.CacheWrite)
		return
	}
	if v.Message.ID != "" {
		a.msgs[v.Message.ID] = len(a.rows)
	}
	a.rows = append(a.rows, t)
}

// totals sums the messages per model.
func (a *usageAcc) totals() []ModelTokens {
	var out []ModelTokens
	idx := make(map[string]int)
	for _, r := range a.rows {
		if r.Input == 0 && r.Output == 0 && r.CacheRead == 0 && r.CacheWrite == 0 {
			continue
		}
		i, ok := idx[r.Model]
		if !ok {
			i = len(out)
			idx[r.Model] = i
			out = append(out, ModelTokens{Model: r.Model})
		}
		o := &out[i]
		o.Input += r.Input
		o.Output += r.Output
		o.CacheRead += r.CacheRead
		o.CacheWrite += r.CacheWrite
	}
	return out
}
