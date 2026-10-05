// cost_reconcile_born.go — name the CLI session a first turn ran from when
// its transcript began, for run records written before the session id was
// known (#3411).
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/session"
)

// birthScanBytes bounds how much of a transcript's head is read for its
// first timestamp and entrypoint.
const birthScanBytes = 4 << 20

// sessionBirths finds the naozhi session a first turn began, read on first
// use: a run record written before its session's id was known names none,
// and a deleted key leaves nothing else naming that session.
type sessionBirths struct {
	claudeDir, storePath string
	spans                []timeSpan // windows of the runs whose record names no session
	read                 bool
	births               []sessionBirth
}

// sessionBirth is when a session's transcript began; ok is false when that
// could not be read, so it may have begun any time up to written.
type sessionBirth struct {
	sid         string
	at, written time.Time
	ok          bool
}

func newSessionBirths(claudeDir, storePath string, at attribution) *sessionBirths {
	b := &sessionBirths{claudeDir: claudeDir, storePath: storePath}
	for id := range at.unnamed {
		if s := at.runs[id]; !s.from.IsZero() && !s.to.IsZero() {
			b.spans = append(b.spans, timeSpan{s.from.Add(-turnStampSlack), s.to.Add(turnStampSlack)})
		}
	}
	return b
}

// soleBornIn names the one naozhi session whose transcript began in
// [from, to], a first turn's window, when no other run without a session
// holds that beginning too. Two sessions begun there, one whose beginning
// could not be read, or a beginning another such run also holds (its own
// transcript gone, a concurrent first turn's) name none.
func (b *sessionBirths) soleBornIn(from, to time.Time) string {
	b.load()
	sole := sessionBirth{}
	for _, s := range b.births {
		if !s.ok {
			if s.written.Before(from) {
				continue
			}
			return ""
		}
		if s.at.Before(from) || s.at.After(to) {
			continue
		}
		if sole.sid != "" {
			return ""
		}
		sole = s
	}
	if sole.sid == "" {
		return ""
	}
	holders := 0
	for _, w := range b.spans {
		if !sole.at.Before(w.from) && !sole.at.After(w.to) {
			holders++
		}
	}
	if holders != 1 {
		return ""
	}
	return sole.sid
}

// load reads the beginning of every naozhi-known session's transcript last
// written no earlier than the earliest window; one begun interactively is
// left out.
func (b *sessionBirths) load() {
	if b.read {
		return
	}
	b.read = true
	if len(b.spans) == 0 {
		return
	}
	earliest := b.spans[0].from
	for _, w := range b.spans[1:] {
		if w.from.Before(earliest) {
			earliest = w.from
		}
	}
	paths := transcriptPaths(b.claudeDir)
	for _, sid := range session.StoredKnownIDs(b.storePath) {
		p := paths[sid]
		if p == "" {
			continue
		}
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.ModTime().Before(earliest) {
			continue
		}
		at, interactive, ok := transcriptBirth(p)
		if interactive {
			continue
		}
		b.births = append(b.births, sessionBirth{sid: sid, at: at, written: fi.ModTime(), ok: ok})
	}
}

// transcriptPaths maps each session id to its main transcript under
// <claudeDir>/projects/*/, the first by path.
func transcriptPaths(claudeDir string) map[string]string {
	out := map[string]string{}
	root := claudefs.ProjectsRoot(claudeDir)
	if root == "" {
		return out
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	sort.Strings(matches)
	for _, p := range matches {
		sid := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if _, seen := out[sid]; !seen && claudefs.IsValidSessionID(sid) {
			out[sid] = p
		}
	}
	return out
}

// transcriptBirth returns the timestamp of the transcript's first line that
// has one, and whether its first line naming an entrypoint names an
// interactive terminal; ok is false when no timestamp was read.
func transcriptBirth(path string) (at time.Time, interactive, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false, false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, birthScanBytes))
	sc.Buffer(nil, birthScanBytes)
	for sc.Scan() {
		var l struct{ Timestamp, Entrypoint string }
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if !ok {
			at, ok = claudefs.ParseTimestamp(l.Timestamp)
		}
		if l.Entrypoint != "" {
			return at, l.Entrypoint == "cli" || l.Entrypoint == "claude-vscode", ok
		}
	}
	return at, false, ok
}
