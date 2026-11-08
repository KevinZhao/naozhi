package digest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// MinGap is how often one session's digest is written at most: a turn
// ending every few seconds must not rewrite it every few seconds.
const MinGap = time.Minute

// Retention is how long a digest outlives its session, as long as the
// session's sidecar: a done task's digest is what the Planner reads about it
// after it was put away.
const Retention = sidecar.Retention

// RetryGap is how long a write waits for a session whose sources are not
// all in yet (Config.Ready) before trying again.
const RetryGap = 10 * time.Second

// sweepInterval is how often Run looks for digests past Retention.
const sweepInterval = time.Hour

// maxStateBytes caps a state file's read: a goal and a report's text.
const maxStateBytes = 256 << 10

// headerScan caps the bytes read to find a digest's frontmatter.
const headerScan = 4 << 10

// State is what the digest keeps of a session beside its file
// (<hash>.json): what cannot be read back from the session later.
type State struct {
	V      int     `json:"v"`
	Key    string  `json:"key"`
	Goal   string  `json:"goal,omitempty"`
	Report *Report `json:"report,omitempty"`
	// Born is the session creation (Facts.Born) Goal and Replies are of.
	// Replies are the latest turns' replies the digest showed, kept so a
	// view holding fewer (a turn longer than the events in view) does not
	// blank them.
	Born    time.Time `json:"born,omitzero"`
	Replies []Reply   `json:"replies,omitempty"`
}

// Config wires a Writer.
type Config struct {
	// Dir is the digest directory (datadir.Layout.PlannerDigestRoot); ""
	// writes nothing.
	Dir string
	// Facts gathers key's digest; false writes none (a session not
	// covered: not observed, not on this node, gone).
	Facts func(key string) (Facts, bool)
	// Ready reports whether key's sources are all in: the health table has
	// judged it, its history from before a restart is loaded. A key not
	// ready is tried again RetryGap later, so a restart does not write
	// "health unknown, no replies" over a digest that had them. nil is
	// always ready.
	Ready func(key string) bool
	// Keys are the sessions to digest when Run starts, so a session that
	// ends no turn after a restart still has one; nil starts with none.
	Keys func() []string
	// Written is told of every digest written (index.json refreshes); nil
	// tells no one.
	Written func(key string)
	// Live reports whether session key still exists; Retired is when its
	// sidecar saw it gone (zero for none). Sweep keeps a live session's
	// digest and drops a gone one's Retention after it went, or after the
	// digest was last written when nothing says when. nil Live sweeps
	// nothing.
	Live    func(key string) bool
	Retired func(key string) time.Time
	// Now is the wall clock; nil is time.Now. MinGap and RetryGap
	// override the package's for tests (zero keeps them).
	Now      func() time.Time
	MinGap   time.Duration
	RetryGap time.Duration
}

// file is what the Writer knows of a digest on disk: when it was written
// and its body's hash, to skip a write that would change nothing.
type file struct {
	updated time.Time
	body    [sha256.Size]byte
}

// Writer writes the digests on one goroutine (Run), each session's at most
// once per MinGap and always once more after its last Touch. Touch, Path,
// State and the setters are safe from any goroutine; every method of a nil
// Writer is a no-op, so the wiring can leave the digests off.
type Writer struct {
	cfg  Config
	poke chan struct{}

	mu     sync.Mutex
	dirty  map[string]struct{}
	files  map[string]file
	states map[string]State
	// held are the state files New could not read (too big, misnamed, not
	// to be moved aside): left as found, never saved over.
	held map[string]bool

	// saveMu orders the state saves, which run off mu: a Touch on a read
	// loop must not wait on the disk.
	saveMu sync.Mutex

	// Run's own.
	last map[string]time.Time
	due  map[string]time.Time
}

// New loads the digests and states under cfg.Dir.
func New(cfg Config) *Writer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MinGap <= 0 {
		cfg.MinGap = MinGap
	}
	if cfg.RetryGap <= 0 {
		cfg.RetryGap = RetryGap
	}
	w := &Writer{
		cfg: cfg, poke: make(chan struct{}, 1),
		dirty: map[string]struct{}{}, files: map[string]file{}, states: map[string]State{}, held: map[string]bool{},
		last: map[string]time.Time{}, due: map[string]time.Time{},
	}
	w.load()
	return w
}

// FileName is key's digest file name.
func FileName(key string) string { return persist.KeyHash(key) + ".md" }

func stateName(key string) string { return persist.KeyHash(key) + ".json" }

func (w *Writer) load() {
	if w.cfg.Dir == "" {
		return
	}
	mds, _ := filepath.Glob(filepath.Join(w.cfg.Dir, "*.md"))
	for _, p := range mds {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		key, updated, body, ok := parseHeader(data)
		if !ok || filepath.Base(p) != FileName(key) {
			slog.Warn("planner digest does not match its name; skipped", "path", p)
			continue
		}
		w.files[key] = file{updated: updated, body: sha256.Sum256(body)}
	}
	sts, _ := filepath.Glob(filepath.Join(w.cfg.Dir, "*.json"))
	for _, p := range sts {
		// A corrupt state is moved aside (PreserveCorrupt) and its session
		// starts a new one; one that cannot be is held, not saved over.
		st, out, err := jsonfile.Load[State](p, jsonfile.Options{MaxBytes: maxStateBytes, Label: "planner digest state"})
		switch {
		case err != nil:
			slog.Warn("planner digest state unreadable; left as is", "path", p, "err", err)
			w.held[filepath.Base(p)] = true
		case out != jsonfile.Parsed:
		case st.Key == "" || filepath.Base(p) != stateName(st.Key):
			slog.Warn("planner digest state does not match its name; left as is", "path", p)
			w.held[filepath.Base(p)] = true
		default:
			w.states[st.Key] = st
		}
	}
}

// parseHeader reads a digest's frontmatter: its key and updated time, and
// the body after it.
func parseHeader(data []byte) (key string, updated time.Time, body []byte, ok bool) {
	head := data[:min(len(data), headerScan)]
	sc := bufio.NewScanner(bytes.NewReader(head))
	if !sc.Scan() || sc.Text() != frontmatterSep {
		return "", time.Time{}, nil, false
	}
	n := len(sc.Bytes()) + 1
	for sc.Scan() {
		line := sc.Text()
		n += len(sc.Bytes()) + 1
		if line == frontmatterSep {
			if key == "" || n > len(data) {
				return "", time.Time{}, nil, false
			}
			return key, updated, data[n:], true
		}
		name, val, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		switch name {
		case "key":
			_ = json.Unmarshal([]byte(val), &key)
		case "updated":
			var s string
			if json.Unmarshal([]byte(val), &s) == nil {
				updated, _ = time.Parse(time.RFC3339, s)
			}
		}
	}
	return "", time.Time{}, nil, false
}

// Touch asks for key's digest to be written. Never blocks; safe on a read
// loop.
func (w *Writer) Touch(key string) {
	if w == nil || key == "" {
		return
	}
	w.mu.Lock()
	w.dirty[key] = struct{}{}
	w.mu.Unlock()
	select {
	case w.poke <- struct{}{}:
	default:
	}
}

// Path is key's digest's absolute path, "" while it has none.
func (w *Writer) Path(key string) string {
	if w == nil || w.cfg.Dir == "" {
		return ""
	}
	w.mu.Lock()
	_, ok := w.files[key]
	w.mu.Unlock()
	if !ok {
		return ""
	}
	return filepath.Join(w.cfg.Dir, FileName(key))
}

// Read is key's digest as on disk and when it was written; false when it
// has none.
func (w *Writer) Read(key string) (updated time.Time, md []byte, ok bool) {
	p := w.Path(key)
	if p == "" {
		return time.Time{}, nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return time.Time{}, nil, false
	}
	k, updated, _, ok := parseHeader(data)
	if !ok || k != key {
		return time.Time{}, nil, false
	}
	return updated, data, true
}

// State is what the digest keeps of key; false when nothing.
func (w *Writer) State(key string) (State, bool) {
	if w == nil {
		return State{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.states[key]
	return st, ok
}

// SetGoal records key's goal (Goal of it), replacing any, and asks for the
// digest to be written.
func (w *Writer) SetGoal(key, goal string) error {
	if w == nil {
		return nil
	}
	goal = Goal(goal)
	_, err := w.update(key, func(st *State) bool {
		if st.Goal == goal {
			return false
		}
		st.Goal = goal
		return true
	})
	w.Touch(key)
	return err
}

// SetReport records key's latest report and asks for the digest to be
// written.
func (w *Writer) SetReport(key string, r Report) error {
	if w == nil {
		return nil
	}
	r.Summary = textutil.TruncateRunes(oneLine(r.Summary), SummaryRunes)
	r.Text = textutil.TruncateRunes(strings.TrimSpace(r.Text), ReportRunes)
	_, err := w.update(key, func(st *State) bool { st.Report = &r; return true })
	w.Touch(key)
	return err
}

// settle folds what f shows of key into its state and returns the state.
// Another Born is a new conversation: the old one's replies go, and its
// goal unless key is a task (whose goal is its dispatch's). The first goal
// seen is kept; the replies are the latest MaxReplies of those kept and
// those in view.
func (w *Writer) settle(key string, f Facts) State {
	st, err := w.update(key, func(st *State) bool {
		changed := false
		if !f.Born.IsZero() && !st.Born.Equal(f.Born) {
			if !st.Born.IsZero() {
				st.Replies = nil
				if !sessionkey.IsTaskKey(key) {
					st.Goal = ""
				}
			}
			st.Born, changed = f.Born, true
		}
		if g := Goal(f.GoalSeen); st.Goal == "" && g != "" {
			st.Goal, changed = g, true
		}
		if r := mergeReplies(st.Replies, f.Replies); !sameReplies(r, st.Replies) {
			st.Replies, changed = r, true
		}
		return changed
	})
	if err != nil {
		slog.Warn("planner digest state not saved", "key", key, "err", err)
	}
	return st
}

// mergeReplies is the latest MaxReplies of kept and seen (both oldest
// first): seen's, after those of kept from before seen's first. A view
// with none keeps kept whole.
func mergeReplies(kept, seen []Reply) []Reply {
	var out []Reply
	for _, r := range kept {
		if len(seen) == 0 || r.At.Before(seen[0].At) {
			out = append(out, r)
		}
	}
	for _, r := range seen {
		out = append(out, Reply{At: r.At, Text: textutil.TruncateRunes(r.Text, ReplyRunes)})
	}
	if len(out) > MaxReplies {
		out = out[len(out)-MaxReplies:]
	}
	return out
}

func sameReplies(a, b []Reply) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].At.Equal(b[i].At) || a[i].Text != b[i].Text {
			return false
		}
	}
	return true
}

// update applies fn to key's state and saves it when fn reports a change;
// it returns the state after.
func (w *Writer) update(key string, fn func(*State) bool) (State, error) {
	if key == "" {
		return State{}, errors.New("digest: empty key")
	}
	w.mu.Lock()
	st, ok := w.states[key]
	if !ok {
		st = State{Key: key}
	}
	if !fn(&st) {
		w.mu.Unlock()
		return st, nil
	}
	st.V = Version
	w.states[key] = st
	w.mu.Unlock()
	return st, w.save(key)
}

// save writes key's state as it is by now, so of two saves racing the
// later one lands with the latest state.
func (w *Writer) save(key string) error {
	if w.cfg.Dir == "" {
		return nil
	}
	w.saveMu.Lock()
	defer w.saveMu.Unlock()
	name := stateName(key)
	w.mu.Lock()
	st, ok := w.states[key]
	held := w.held[name]
	w.mu.Unlock()
	if !ok {
		return nil // swept meanwhile
	}
	if held {
		return fmt.Errorf("digest: state file %s could not be read; left as is", name)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		err = datadir.EnsureDir(w.cfg.Dir)
	}
	if err == nil {
		err = osutil.WriteFileAtomic(filepath.Join(w.cfg.Dir, name), data, 0o600)
	}
	return err
}

// Run writes the digests asked for until ctx ends: at once for a session
// not written within MinGap, else MinGap after its last write, so the last
// Touch of a burst is always written. It sweeps at once and hourly.
func (w *Writer) Run(ctx context.Context) {
	if w == nil {
		return
	}
	if w.cfg.Keys != nil {
		for _, k := range w.cfg.Keys() {
			w.Touch(k)
		}
	}
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()
	w.Sweep()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		next := w.step(w.cfg.Now())
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if !next.IsZero() {
			timer.Reset(max(next.Sub(w.cfg.Now()), time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return
		case <-w.poke:
		case <-timer.C:
		case <-sweep.C:
			w.Sweep()
		}
	}
}

// step schedules the sessions touched since the last step and writes those
// due by now; it returns when the next one is due (zero for none).
func (w *Writer) step(now time.Time) time.Time {
	w.mu.Lock()
	dirty := w.dirty
	w.dirty = map[string]struct{}{}
	w.mu.Unlock()
	for key := range dirty {
		at := now
		if last, ok := w.last[key]; ok && now.Sub(last) < w.cfg.MinGap {
			at = last.Add(w.cfg.MinGap)
		}
		if old, ok := w.due[key]; !ok || at.Before(old) {
			w.due[key] = at
		}
	}
	var next time.Time
	for key, at := range w.due {
		if at.After(now) {
			if next.IsZero() || at.Before(next) {
				next = at
			}
			continue
		}
		if w.cfg.Ready != nil && !w.cfg.Ready(key) {
			retry := now.Add(w.cfg.RetryGap)
			w.due[key] = retry
			if next.IsZero() || retry.Before(next) {
				next = retry
			}
			continue
		}
		delete(w.due, key)
		if w.write(key, now) {
			w.last[key] = now
		}
	}
	return next
}

// write writes key's digest as of now; true when the file changed.
func (w *Writer) write(key string, now time.Time) bool {
	if w.cfg.Dir == "" || w.cfg.Facts == nil {
		return false
	}
	f, ok := w.cfg.Facts(key)
	if !ok {
		return false
	}
	f.Key = key
	st := w.settle(key, f)
	f.Goal, f.Replies, f.Report = st.Goal, st.Replies, st.Report
	body := renderBody(f)
	sum := sha256.Sum256(body)
	w.mu.Lock()
	old, had := w.files[key]
	w.mu.Unlock()
	if had && old.body == sum {
		return false
	}
	f.Updated = now
	var b bytes.Buffer
	writeHeader(&b, key, now)
	b.Write(body)
	err := datadir.EnsureDir(w.cfg.Dir)
	if err == nil {
		err = osutil.WriteFileAtomic(filepath.Join(w.cfg.Dir, FileName(key)), b.Bytes(), 0o600)
	}
	if err != nil {
		slog.Warn("planner digest not written", "key", key, "err", err)
		return false
	}
	w.mu.Lock()
	w.files[key] = file{updated: now, body: sum}
	w.mu.Unlock()
	if w.cfg.Written != nil {
		w.cfg.Written(key)
	}
	return true
}

// Sweep removes the digests (and states) of sessions gone longer than
// Retention: since their sidecar saw them go, else since the digest was
// last written (or the state saved, for one never written).
func (w *Writer) Sweep() {
	if w == nil || w.cfg.Live == nil || w.cfg.Dir == "" {
		return
	}
	now := w.cfg.Now()
	w.mu.Lock()
	keys := map[string]time.Time{}
	for k, f := range w.files {
		keys[k] = f.updated
	}
	for k := range w.states {
		if _, ok := keys[k]; !ok {
			keys[k] = time.Time{}
		}
	}
	w.mu.Unlock()
	for key, updated := range keys {
		if w.cfg.Live(key) {
			continue
		}
		since := time.Time{}
		if w.cfg.Retired != nil {
			since = w.cfg.Retired(key)
		}
		if since.IsZero() {
			since = updated
		}
		if since.IsZero() {
			if fi, err := os.Stat(filepath.Join(w.cfg.Dir, stateName(key))); err == nil {
				since = fi.ModTime()
			}
		}
		if since.IsZero() || now.Sub(since) <= Retention {
			continue
		}
		w.remove(key)
	}
}

func (w *Writer) remove(key string) {
	w.saveMu.Lock()
	defer w.saveMu.Unlock()
	w.mu.Lock()
	delete(w.files, key)
	delete(w.states, key)
	names := []string{FileName(key)}
	if !w.held[stateName(key)] {
		names = append(names, stateName(key))
	}
	w.mu.Unlock()
	for _, name := range names {
		if err := os.Remove(filepath.Join(w.cfg.Dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("planner digest not removed", "path", name, "err", err)
		}
	}
}
