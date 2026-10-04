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
	acc := newUsageAcc(w)
	found, _, err = readSessionUsage(projectDir, sessionID, w, acc)
	if err != nil || !found {
		return nil, found, err
	}
	return acc.totals(), true, nil
}

// DailyUsage is a session's transcript usage split per UTC day.
type DailyUsage struct {
	// Days maps a UTC day (2006-01-02) to its per-model rows, in first-seen
	// model order; a message counts on the day of its first line.
	Days map[string][]ModelTokens
	// Truncated is set when the session has more agent transcripts than one
	// call reads, so the figures are a lower bound.
	Truncated bool
}

// SessionDailyUsage is SessionUsage over a session's whole history, per UTC
// day. A message whose id is in counted was already counted for another
// session (a fork copies its parent's lines) and is skipped; the ids counted
// here are added to counted.
func SessionDailyUsage(projectDir, sessionID string, counted map[string]bool) (u DailyUsage, found bool, err error) {
	acc := newUsageAcc(UsageWindow{})
	acc.skip = counted
	found, u.Truncated, err = readSessionUsage(projectDir, sessionID, UsageWindow{}, acc)
	if err != nil || !found {
		return DailyUsage{}, found, err
	}
	for id := range acc.msgs {
		counted[id] = true
	}
	u.Days = acc.dailyTotals()
	return u, true, nil
}

// readSessionUsage feeds the session's main transcript and its agent
// transcripts to acc. truncated reports agent transcripts left unread.
func readSessionUsage(projectDir, sessionID string, w UsageWindow, acc *usageAcc) (found, truncated bool, err error) {
	if projectDir == "" || !IsValidSessionID(sessionID) {
		return false, false, nil
	}
	f, err := os.Open(TranscriptIn(projectDir, sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	err = readUsageFrom(f, w.MainOffset, acc)
	f.Close()
	if err != nil {
		return true, false, err
	}
	paths, truncated := agentTranscripts(SubagentsDir(projectDir, sessionID), w.Since)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue // removed meanwhile, or unreadable: its usage is lost, the rest still counts
		}
		err = readUsageFrom(f, 0, acc)
		f.Close()
		if err != nil {
			return true, truncated, err
		}
	}
	return true, truncated, nil
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
// dir modified at or after since, at most maxUsageFiles of them; truncated
// reports that more qualified. Only regular files count, so a symlink planted
// in the tree is never followed.
func agentTranscripts(dir string, since time.Time) (out []string, truncated bool) {
	if dir == "" {
		return nil, false
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
				truncated = true
				return
			}
			out = append(out, filepath.Join(d, name))
		}
	}
	add(dir)
	wfs, _ := os.ReadDir(filepath.Join(dir, "workflows"))
	for _, wf := range wfs {
		if wf.IsDir() {
			add(filepath.Join(dir, "workflows", wf.Name()))
		}
	}
	return out, truncated
}

// usageAcc keeps the usage last seen per API message in the window.
type usageAcc struct {
	sinceMS, untilMS int64
	msgs             map[string]int // message id -> index in rows
	rows             []usageRow
	skip             map[string]bool // message ids counted elsewhere
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
	if v.Message.ID != "" && a.skip[v.Message.ID] {
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
	a.rows = append(a.rows, usageRow{ModelTokens: t, ms: ts})
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

// dailyTotals sums the messages per UTC day and model.
func (a *usageAcc) dailyTotals() map[string][]ModelTokens {
	out := make(map[string][]ModelTokens)
	idx := make(map[string]map[string]int)
	for _, r := range a.rows {
		day := time.UnixMilli(r.ms).UTC().Format(time.DateOnly)
		if idx[day] == nil {
			idx[day] = make(map[string]int)
		}
		if rows := addTokens(out[day], idx[day], r.ModelTokens); len(rows) > 0 {
			out[day] = rows
		}
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
