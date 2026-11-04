package scout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// Sources a finding names.
const (
	SourceEmail    = "email"
	SourceCalendar = "calendar"
	SourceSlack    = "slack"
	SourceAsana    = "asana"
)

// Kinds of finding (§15.6).
const (
	KindReplyNeeded = "reply_needed"
	KindConflict    = "conflict"
	KindDeadline    = "deadline"
	KindMeeting     = "meeting"
	KindFYI         = "fyi"
)

var (
	sources = map[string]bool{SourceEmail: true, SourceCalendar: true, SourceSlack: true, SourceAsana: true}
	kinds   = map[string]bool{KindReplyNeeded: true, KindConflict: true, KindDeadline: true, KindMeeting: true, KindFYI: true}
)

// Limits on what one run's output may carry.
const (
	maxOutputBytes = 512 << 10
	maxFindings    = 100
	maxEvents      = 100
	maxAttendees   = 50
	idRunes        = 512
	fieldRunes     = 500
)

// Finding is one thing a scout run found that may need the user.
type Finding struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	From   string `json:"from"`
	At     string `json:"at"`
	Due    string `json:"due"`
	Link   string `json:"link"`
	Why    string `json:"why"`
}

// key is the finding's dedup key: (source, ID, kind).
func (f Finding) key() string { return f.Source + "\x00" + f.ID + "\x00" + f.Kind }

// Cursors are where each source was last read up to: a time or an ID, as
// the scout session wrote it; "" is never read.
type Cursors struct {
	Email string `json:"email"`
	Slack string `json:"slack"`
	Asana string `json:"asana"`
}

// merge takes next's non-empty cursors over c's.
func (c Cursors) merge(next Cursors) Cursors {
	if next.Email != "" {
		c.Email = next.Email
	}
	if next.Slack != "" {
		c.Slack = next.Slack
	}
	if next.Asana != "" {
		c.Asana = next.Asana
	}
	return c
}

// Event is a calendar entry of the next 24 hours.
type Event struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Organizer string    `json:"organizer"`
	Attendees []string  `json:"attendees"`
	Online    bool      `json:"online"`
	Link      string    `json:"link"`
	// Prep marks a meeting the user should prepare for (speaks, brings
	// material, decides); only those get a pre-meeting brief. Broadcast
	// sessions and office hours do not.
	Prep bool `json:"prep,omitempty"`
}

// Output is a scout session's whole reply.
type Output struct {
	Findings []Finding `json:"findings"`
	Cursors  Cursors   `json:"cursors"`
	// Calendar is nil when the session did not read it (the key left out).
	Calendar *[]Event `json:"calendar"`
}

// events is the calendar read, none when it was not.
func (o Output) events() []Event {
	if o.Calendar == nil {
		return nil
	}
	return *o.Calendar
}

// errNoOutput is a reply with no JSON object in it.
var errNoOutput = errors.New("reply holds no JSON object")

// decodeReply decodes a scout session's reply into v strictly: one JSON
// object, optionally inside a single ```json fence, with no unknown field
// and no trailing data.
func decodeReply(reply string, v any) error {
	text := strings.TrimSpace(reply)
	if rest, ok := strings.CutPrefix(text, "```"); ok {
		rest = strings.TrimPrefix(rest, "json")
		body, ok := strings.CutSuffix(strings.TrimSpace(rest), "```")
		if !ok {
			return errors.New("unterminated code fence")
		}
		text = strings.TrimSpace(body)
	}
	if !strings.HasPrefix(text, "{") {
		return errNoOutput
	}
	if len(text) > maxOutputBytes {
		return fmt.Errorf("reply is %d bytes, over %d", len(text), maxOutputBytes)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON object")
	}
	return nil
}

// parseOutput reads a scout session's reply strictly: one JSON object,
// optionally inside a single ```json fence, with no unknown field and no
// trailing data; every finding names a known source and kind, an ID and a
// title, and every event an ID and its start and end; the calendar key is
// left out when the session did not read it. Anything else fails
// the run, which then leaves the cursors where they were.
func parseOutput(reply string) (Output, error) {
	var out Output
	if err := decodeReply(reply, &out); err != nil {
		return Output{}, err
	}
	if len(out.Findings) > maxFindings || len(out.events()) > maxEvents {
		return Output{}, fmt.Errorf("%d findings, %d events: over %d / %d", len(out.Findings), len(out.events()), maxFindings, maxEvents)
	}
	for i := range out.Findings {
		f := &out.Findings[i]
		switch {
		case !sources[f.Source]:
			return Output{}, fmt.Errorf("finding %d: unknown source %q", i, f.Source)
		case !kinds[f.Kind]:
			return Output{}, fmt.Errorf("finding %d: unknown kind %q", i, f.Kind)
		case strings.TrimSpace(f.ID) == "" || strings.TrimSpace(f.Title) == "":
			return Output{}, fmt.Errorf("finding %d: no id or title", i)
		}
		f.ID = strings.TrimSpace(f.ID)
		if f.Kind == KindConflict {
			f.ID = conflictID(f.ID, f.At)
		}
		f.ID = textutil.TruncateRunes(f.ID, idRunes)
		for _, s := range []*string{&f.Title, &f.From, &f.At, &f.Due, &f.Link, &f.Why} {
			*s = textutil.TruncateRunes(strings.TrimSpace(*s), fieldRunes)
		}
	}
	for i := range out.events() {
		e := &(*out.Calendar)[i]
		if strings.TrimSpace(e.ID) == "" || e.Start.IsZero() || e.End.IsZero() {
			return Output{}, fmt.Errorf("event %d: no id, start or end", i)
		}
		e.ID = textutil.TruncateRunes(strings.TrimSpace(e.ID), idRunes)
		for _, s := range []*string{&e.Title, &e.Organizer, &e.Link} {
			*s = textutil.TruncateRunes(strings.TrimSpace(*s), fieldRunes)
		}
		if len(e.Attendees) > maxAttendees {
			e.Attendees = e.Attendees[:maxAttendees]
		}
	}
	for _, c := range []*string{&out.Cursors.Email, &out.Cursors.Slack, &out.Cursors.Asana} {
		*c = textutil.TruncateRunes(strings.TrimSpace(*c), idRunes)
	}
	return out, nil
}

// conflictID is a conflict's dedup ID. A session copies the long Outlook
// meeting IDs it joins unreliably from run to run (a character dropped,
// another ID of the same meeting, a '+' inside the base64), so a conflict
// with a parseable start is known by that time; without one, by its
// meetings' IDs sorted, so either order dedups as one.
func conflictID(id, at string) string {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(at)); err == nil {
		return "at:" + t.UTC().Format(time.RFC3339)
	}
	parts := strings.Split(id, "+")
	for j := range parts {
		parts[j] = strings.TrimSpace(parts[j])
	}
	slices.Sort(parts)
	return strings.Join(parts, "+")
}
