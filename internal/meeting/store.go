package meeting

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// States of a meeting.
const (
	StateRecording  = "recording"
	StateProcessing = "processing"
	StateDone       = "done"
	StateFailed     = "failed"
)

// AudioRetention is how long the audio is kept after the summary (§6; a constant).
const AudioRetention = 30 * 24 * time.Hour

// Meeting is a meeting's meta.json.
type Meeting struct {
	ID string `json:"id"`
	// Title is the calendar event's, else "<app> 会议 HH:MM".
	Title    string    `json:"title"`
	App      string    `json:"app"`
	Bundles  []string  `json:"bundles,omitempty"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitzero"`
	State    string    `json:"state"`
	Stage    string    `json:"stage,omitempty"`
	Error    string    `json:"error,omitempty"`
	StopBy   string    `json:"stop_by,omitempty"`
	Segments int       `json:"segments"`
	Seconds  float64   `json:"seconds"`
	Parts    int       `json:"parts"`
	// LastSegment is when the last segment was written: a recording a restarted gateway left
	// behind is resumed by the same meeting only shortly after it.
	LastSegment time.Time `json:"last_segment,omitzero"`
	Attendees   []string  `json:"attendees,omitempty"`
	// Silent is set when the far-end track never rose above silence: the usual sign of a missing
	// system-audio grant, which Core Audio answers with zeros rather than an error.
	Silent       bool      `json:"silent,omitempty"`
	MicSilent    bool      `json:"mic_silent,omitempty"`
	Todos        []Todo    `json:"todos,omitempty"`
	KeepAudio    bool      `json:"keep_audio,omitempty"`
	AudioDeleted bool      `json:"audio_deleted,omitempty"`
	AudioExpires time.Time `json:"audio_expires,omitzero"`
	Speakers     int       `json:"speakers,omitempty"`
	// Language is the recognition language the user chose ("" lets the engine pick per track).
	Language string `json:"language,omitempty"`
	Engine   string `json:"engine,omitempty"`
}

// Todo is one to-do the summary found. Mine are the user's own; they become promises.
type Todo struct {
	Owner   string `json:"owner"`
	Text    string `json:"text"`
	Due     string `json:"due,omitempty"`
	Mine    bool   `json:"mine"`
	Promise string `json:"promise,omitempty"`
}

var idRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-[0-9a-f]{6}$`)

// ValidID reports whether id names a meeting directory.
func ValidID(id string) bool { return idRE.MatchString(id) }

// Store is the meetings directory: one directory per meeting, meta.json written atomically.
type Store struct {
	Root string
	mu   sync.Mutex
}

func newID(t time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return t.Format("2006-01-02") + "-" + hex.EncodeToString(b[:])
}

// Dir is a meeting's directory.
func (s *Store) Dir(id string) string { return filepath.Join(s.Root, id) }

// Create makes the directory of a new meeting started at t.
func (s *Store) Create(m Meeting) (Meeting, error) {
	if s.Root == "" {
		return Meeting{}, errors.New("no state root")
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return Meeting{}, err
	}
	for range 5 {
		m.ID = newID(m.Started)
		if err := os.Mkdir(s.Dir(m.ID), 0o700); err == nil {
			return m, s.Save(m)
		} else if !os.IsExist(err) {
			return Meeting{}, err
		}
	}
	return Meeting{}, errors.New("no free meeting ID")
}

// Save writes meta.json.
func (s *Store) Save(m Meeting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(filepath.Join(s.Dir(m.ID), "meta.json"), m)
}

// Get reads one meeting.
func (s *Store) Get(id string) (Meeting, error) {
	if !ValidID(id) {
		return Meeting{}, os.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(id), "meta.json"))
	if err != nil {
		return Meeting{}, err
	}
	var m Meeting
	return m, json.Unmarshal(b, &m)
}

// Update reads, changes and writes a meeting under the store's lock.
func (s *Store) Update(id string, fn func(*Meeting)) (Meeting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(s.Dir(id), "meta.json"))
	if err != nil {
		return Meeting{}, err
	}
	var m Meeting
	if err := json.Unmarshal(b, &m); err != nil {
		return Meeting{}, err
	}
	fn(&m)
	return m, writeJSON(filepath.Join(s.Dir(id), "meta.json"), m)
}

// List is every meeting, newest first.
func (s *Store) List() []Meeting {
	ents, err := os.ReadDir(s.Root)
	if err != nil {
		return nil
	}
	var out []Meeting
	for _, e := range ents {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		if m, err := s.Get(e.Name()); err == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// segments lists a track's complete segments in order.
func (s *Store) segments(id, track string) []string {
	ents, err := os.ReadDir(filepath.Join(s.Dir(id), "audio", track))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".wav") {
			out = append(out, filepath.Join(s.Dir(id), "audio", track, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, b)
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
