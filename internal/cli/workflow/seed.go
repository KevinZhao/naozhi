package workflow

import (
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
	maxSeedTaskIDLen     = 32
)

// Seed frame classes: each task needs only its newest line of each, since
// every one of them supersedes the older ones.
const (
	classSnapshot = iota // task_progress with a snapshot
	classHeader          // task_progress without one
	classUpdated
	classNotification
	numClasses
	classAlways // task_started and launch lines: always decoded
)

// SeedFromReplay builds the Tracker from a reconnected process's replay
// before its read loop starts. known are task ids the session already
// knows as workflows. Replay frames carry no time, so nothing seeded gets
// LastObservedAt or a StartedAt from observation.
//
// A reverse walk picks lines by prefix alone, decoding none: per task the
// newest line of each class, plus every task_started and launch line. The
// picked lines are then decoded and applied oldest first, as live frames
// would be — applying them newest first would let a terminal frame shut
// out the snapshot before it. Decodes are O(tasks), not O(lines).
func (t *Tracker) SeedFromReplay(r Replay, dec Decoder, known []string) {
	t.KnowTasks(known)
	type pick struct {
		line   int
		events []clievent.Event // decoded already when the id needed it
	}
	var picks []pick
	filled := map[string]*[numClasses]bool{}
	for i := len(r.Lines) - 1; i >= 0; i-- {
		line := r.Lines[i]
		class, ok := seedClass(line)
		if !ok {
			continue
		}
		if class == classAlways {
			picks = append(picks, pick{line: i})
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
		slots := filled[id]
		if slots == nil {
			slots = &[numClasses]bool{}
			filled[id] = slots
		}
		if slots[class] {
			continue
		}
		slots[class] = true
		picks = append(picks, pick{line: i, events: events})
	}

	t.mu.Lock()
	for k := len(picks) - 1; k >= 0; k-- {
		events := picks[k].events
		if events == nil {
			events, _, _ = dec.ReadEvent(r.Lines[picks[k].line])
		}
		for j := range events {
			if kind := kindOf(&events[j]); kind != kindNone {
				t.observeLocked(&events[j], kind, 0, SourceReplay)
			}
		}
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
		return classUpdated, true
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
