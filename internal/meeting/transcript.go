package meeting

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Segment is one recognised stretch of speech, seconds from the start of the track.
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Hints is context for recognition (§3.5 热词): the calendar title, attendee names and the like.
type Hints struct {
	Title     string
	Attendees []string
	// Language pins recognition ("Chinese", "English"); "" lets the engine pick per track.
	Language string
}

// Languages a meeting can be re-recognised in; "" is the engine's own pick.
var Languages = []string{"", "Chinese", "English"}

// Recognizer turns one 16 kHz mono WAV track into segments. Implementations: the local engine
// (LocalEngine) and, later, cloud ones (§3.5: a pluggable interface).
type Recognizer interface {
	Name() string
	Transcribe(ctx context.Context, wav string, hints Hints) ([]Segment, error)
}

// Turn is one speaker turn of a diarised track.
type Turn struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker string  `json:"speaker"`
}

// Diarizer splits the far-end track into speakers. nil leaves every far-end line to "对方".
type Diarizer interface {
	Diarize(ctx context.Context, wav string) ([]Turn, error)
}

// Line is one line of the transcript.
type Line struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker string  `json:"speaker"`
	Text    string  `json:"text"`
}

// Speaker labels.
const (
	SpeakerMe     = "你"
	SpeakerRemote = "对方"
)

// Merge puts the two tracks into one timeline: the microphone is the user, the far end is split
// by turns when there are any (most overlap wins), labelled 说话人 A, B, ... in order of first
// appearance.
func Merge(mine, remote []Segment, turns []Turn) ([]Line, int) {
	var out []Line
	for _, s := range mine {
		if t := strings.TrimSpace(s.Text); t != "" {
			out = append(out, Line{Start: s.Start, End: s.End, Speaker: SpeakerMe, Text: t})
		}
	}
	labels := map[string]string{}
	for _, s := range remote {
		t := strings.TrimSpace(s.Text)
		if t == "" {
			continue
		}
		who := SpeakerRemote
		if sp := dominant(s, turns); sp != "" {
			l, ok := labels[sp]
			if !ok {
				l = "说话人 " + letter(len(labels))
				labels[sp] = l
			}
			who = l
		}
		out = append(out, Line{Start: s.Start, End: s.End, Speaker: who, Text: t})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, len(labels)
}

func letter(i int) string {
	if i < 26 {
		return string(rune('A' + i))
	}
	return fmt.Sprintf("%d", i+1)
}

func dominant(s Segment, turns []Turn) string {
	best, bestOv := "", 0.0
	for _, t := range turns {
		ov := min(s.End, t.End) - max(s.Start, t.Start)
		if ov > bestOv {
			best, bestOv = t.Speaker, ov
		}
	}
	return best
}

// Clock formats seconds as [h:]mm:ss.
func Clock(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// TranscriptMarkdown renders transcript.md: consecutive lines of one speaker are joined.
func TranscriptMarkdown(m Meeting, lines []Line) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s · 文字稿\n\n", m.Title)
	fmt.Fprintf(&b, "%s · %s\n\n", m.Started.Local().Format("2006-01-02 15:04"), Clock(m.Seconds))
	prev := ""
	for _, l := range lines {
		if l.Speaker != prev {
			fmt.Fprintf(&b, "\n**%s** `%s`\n\n", l.Speaker, Clock(l.Start))
			prev = l.Speaker
		}
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}
