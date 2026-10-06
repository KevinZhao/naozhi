package workflow

import (
	"slices"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Decoder decodes one stream-json line; cli.Protocol satisfies it.
type Decoder interface {
	ReadEvent(line string) (events []clievent.Event, done bool, err error)
}

// Replay is the backlog a reconnected shim hands back, oldest first.
type Replay struct {
	Lines []string
	// Wrapped: the shim ring had already evicted frames this replay
	// should have started with.
	Wrapped bool
}

const (
	seedSystemTaskPrefix = `{"type":"system","subtype":"task_`
	seedUserPrefix       = `{"type":"user"`
	seedLaunchMarker     = `"async_launched"`
	seedSnapshotMarker   = `"` + clievent.WorkflowProgressKey + `":[`
	seedTaskIDKey        = `"task_id":"`
	seedPatchKey         = `"patch":{`
	maxSeedTaskIDLen     = 32
	// maxSeedFallback older lines of a class are tried, newest first, when
	// a task's newest one does not decode (or its snapshot fails the
	// identity check): live kept the older one.
	maxSeedFallback = 3
)

// Seed frame classes: each task needs only its newest line of each, since
// every one of them supersedes the older ones. A task_updated patch holds
// only the fields that changed, so one without a status supersedes none.
const (
	classSnapshot = iota // task_progress with a snapshot
	classHeader          // task_progress without one
	classStatus          // task_updated whose patch sets the status
	classPatch           // any other task_updated
	classNotification
	numClasses
	classAlways // task_started and launch lines: always decoded
)

// SeedFromReplay builds the Tracker from a reconnected process's replay
// before its read loop starts. known are task ids the session already
// knows as workflows. Replay frames carry no time, so nothing seeded gets
// LastObservedAt or a StartedAt from observation.
//
// A reverse walk picks lines by prefix alone: per task the newest line of
// each class, plus every task_started and launch line. The picks are then
// decoded and applied oldest first, as live frames would be — applying
// them newest first would let a terminal frame shut out the snapshot
// before it. A pick that does not decode falls back to an older line of
// its class, as live kept that one. Decodes are O(tasks), not O(lines).
func (t *Tracker) SeedFromReplay(r Replay, dec Decoder, known []string) {
	t.KnowTasks(known)
	type pick struct {
		line   int
		class  int
		id     string
		events []clievent.Event // nil until decoded; snapshots decode last
	}
	type slot struct {
		id    string
		class int
	}
	var picks []pick
	filled := map[slot]bool{}
	older := map[slot][]int{} // up to maxSeedFallback older lines per slot
	firstSnap := map[string]int{}
	for i := len(r.Lines) - 1; i >= 0; i-- {
		line := r.Lines[i]
		class, ok := seedClass(line)
		if !ok {
			continue
		}
		if class == classAlways {
			picks = append(picks, pick{line: i, class: class})
			continue
		}
		id, ok := seedTaskID(line)
		var events []clievent.Event
		if !ok {
			events, _, _ = dec.ReadEvent(line)
			if len(events) == 0 {
				continue
			}
			id = events[0].TaskID
		}
		if class == classSnapshot {
			firstSnap[id] = i
		}
		k := slot{id, class}
		if filled[k] {
			if len(older[k]) < maxSeedFallback {
				older[k] = append(older[k], i)
			}
			continue
		}
		filled[k] = true
		picks = append(picks, pick{line: i, class: class, id: id, events: events})
	}
	// Small picks are decoded up front, so one that fails can fall back to
	// an older line applied at its own place.
	for k := range picks {
		p := &picks[k]
		if p.class == classSnapshot || p.class == classAlways {
			continue
		}
		if p.events == nil {
			p.events, _, _ = dec.ReadEvent(r.Lines[p.line])
		}
		for _, i := range older[slot{p.id, p.class}] {
			if len(p.events) > 0 {
				break
			}
			if prev, _, _ := dec.ReadEvent(r.Lines[i]); len(prev) > 0 {
				p.line, p.events = i, prev
			}
		}
	}
	slices.SortFunc(picks, func(a, b pick) int { return b.line - a.line })
	// A snapshot line is rule 3's evidence from its place on, even when
	// only a newer one of its task is decoded.
	type mark struct {
		line int
		id   string
	}
	marks := make([]mark, 0, len(firstSnap))
	for id, i := range firstSnap {
		marks = append(marks, mark{i, id})
	}
	slices.SortFunc(marks, func(a, b mark) int { return a.line - b.line })

	t.mu.Lock()
	apply := func(events []clievent.Event) {
		for j := range events {
			if kind := kindOf(&events[j]); kind != kindNone {
				t.observeLocked(&events[j], kind, 0, SourceReplay)
			}
		}
	}
	for k := len(picks) - 1; k >= 0; k-- {
		p := picks[k]
		for len(marks) > 0 && marks[0].line < p.line {
			t.rememberLocked(marks[0].id, flagWorkflow)
			marks = marks[1:]
		}
		events := p.events
		if events == nil {
			events, _, _ = dec.ReadEvent(r.Lines[p.line])
		}
		if p.class == classSnapshot && (len(events) == 0 || snapshotFailed(events)) {
			// Live kept an older snapshot's rows. Applied right before this
			// one, whose header then supersedes the frames in between; one
			// that did not decode has none, so the task's header pick, when
			// newer than the fallback, is applied again.
			for _, i := range older[slot{p.id, classSnapshot}] {
				prev, _, _ := dec.ReadEvent(r.Lines[i])
				if len(prev) == 0 || snapshotFailed(prev) {
					continue
				}
				apply(prev)
				if h := slices.IndexFunc(picks, func(q pick) bool {
					return q.id == p.id && q.class == classHeader && q.line > i
				}); h >= 0 && len(events) == 0 {
					apply(picks[h].events)
				}
				break
			}
		}
		apply(events)
	}
	t.seedWrapped = r.Wrapped
	t.publishLocked()
	t.mu.Unlock()
	t.notify()
}

// seedClass classifies a replay line by its prefix; false for lines the
// seed never needs.
func seedClass(line string) (int, bool) {
	if strings.HasPrefix(line, seedUserPrefix) {
		return classAlways, strings.Contains(line, seedLaunchMarker)
	}
	rest, ok := strings.CutPrefix(line, seedSystemTaskPrefix)
	if !ok {
		return 0, false
	}
	switch {
	case strings.HasPrefix(rest, `started"`):
		return classAlways, true
	case strings.HasPrefix(rest, `progress"`):
		if strings.Contains(rest, seedSnapshotMarker) {
			return classSnapshot, true
		}
		return classHeader, true
	case strings.HasPrefix(rest, `updated"`):
		if _, patch, ok := strings.Cut(rest, seedPatchKey); ok && strings.Contains(patch, `"status":`) {
			return classStatus, true
		}
		return classPatch, true
	case strings.HasPrefix(rest, `notification"`):
		return classNotification, true
	}
	return 0, false
}

// seedTaskID cuts the task id out of a task_* line without decoding it.
// Quotes inside JSON strings are escaped, so the first unescaped
// `"task_id":"` is a key; an id outside [a-z0-9]{1,32} is not trusted.
func seedTaskID(line string) (string, bool) {
	i := strings.Index(line, seedTaskIDKey)
	if i < 0 {
		return "", false
	}
	rest := line[i+len(seedTaskIDKey):]
	end := strings.IndexByte(rest, '"')
	if end < 1 || end > maxSeedTaskIDLen {
		return "", false
	}
	for j := 0; j < end; j++ {
		if c := rest[j]; (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "", false
		}
	}
	return rest[:end], true
}

func snapshotFailed(events []clievent.Event) bool {
	return slices.ContainsFunc(events, func(ev clievent.Event) bool {
		return ev.WorkflowDecode == clievent.WorkflowDecodeFailed
	})
}
