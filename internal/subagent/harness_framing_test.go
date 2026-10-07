package subagent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const probeAgentFixture = "testdata/agent-a2093755b9a9ce8c0.jsonl"

func TestStripHarnessFraming(t *testing.T) {
	t.Parallel()
	frame := harnessMark + " The task text below was computed at runtime. The computed task text follows:\n"
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"framed", frame + "  Reply with just the number 2+2", "Reply with just the number 2+2", true},
		{"lines lose two spaces at most", frame + "  first\n    nested\n plain\nflush", "first\n  nested\nplain\nflush", true},
		{"no frame", "  Reply with just the number 2+2", "  Reply with just the number 2+2", false},
		{"mark without its end", harnessMark + " follows: the task", harnessMark + " follows: the task", false},
		{"mark not leading", "quoted " + frame + "  x", "quoted " + frame + "  x", false},
	}
	for _, c := range cases {
		got, ok := StripHarnessFraming(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestTranscriptReader_StripsHarnessFraming reads the probe's workflow agent
// transcript: with the option its first entry is the task, without it the
// framed prompt; a later user line keeps its text either way.
func TestTranscriptReader_StripsHarnessFraming(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(probeAgentFixture)
	if err != nil {
		t.Fatal(err)
	}
	later := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"` + harnessMark + ` x follows:\n  y"}]},"sessionId":"s","timestamp":"2026-10-05T03:13:41Z"}`
	path := tmpFile(t, string(data)+later+"\n")
	read := func(opts ReaderOpts) []string {
		r := NewTranscriptReaderFrom(path, nil, func() (*os.File, error) { return os.Open(path) }, opts)
		defer r.Close() //nolint:errcheck
		ents, err := r.Read(0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range ents {
			out = append(out, e.Detail)
		}
		return out
	}
	got := read(ReaderOpts{StripHarnessFraming: true})
	if len(got) != 3 || got[0] != "Reply with just the number 2+2" || got[1] != "4" || !strings.HasPrefix(got[2], harnessMark) {
		t.Errorf("stripped: %q", got)
	}
	if raw := read(ReaderOpts{}); len(raw) != 3 || !strings.HasPrefix(raw[0], harnessMark) {
		t.Errorf("unstripped: %q", raw)
	}
}

// TestTranscriptReader_StripsAfterTheFirstLineArrives: the first line's
// framing goes even when it lands in a later Tail than the reader's first.
func TestTranscriptReader_StripsAfterTheFirstLineArrives(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(probeAgentFixture)
	if err != nil {
		t.Fatal(err)
	}
	first, rest, _ := strings.Cut(string(data), "\n")
	path := tmpFile(t, first[:100])
	r := NewTranscriptReaderFrom(path, nil, func() (*os.File, error) { return os.Open(path) }, ReaderOpts{StripHarnessFraming: true})
	defer r.Close() //nolint:errcheck
	if ents, err := r.Tail(); err != nil || len(ents) != 0 {
		t.Fatalf("half a line: %v, %v", ents, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(first[100:] + "\n" + rest); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ents, err := r.Tail()
	if err != nil || len(ents) != 2 || ents[0].Summary != "Reply with just the number 2+2" {
		t.Errorf("Tail: %+v, %v", ents, err)
	}
}

// TestNewTranscriptReaderFrom_OpensOnlyThroughOpener: a handed fd is read
// first, and every open after it goes through open, never the path.
func TestNewTranscriptReaderFrom_OpensOnlyThroughOpener(t *testing.T) {
	t.Parallel()
	line := func(text string) string {
		return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}` + "\n"
	}
	elsewhere := tmpFile(t, line("from the opener"))
	handed := tmpFile(t, line("from the handed fd"))
	missing := filepath.Join(t.TempDir(), "agent-none.jsonl")
	opens := 0
	open := func() (*os.File, error) { opens++; return os.Open(elsewhere) }

	r := NewTranscriptReaderFrom(missing, nil, open, ReaderOpts{})
	ents, err := r.Read(0, 0)
	if err != nil || len(ents) != 1 || ents[0].Summary != "from the opener" || opens != 1 {
		t.Errorf("first open: %+v, %v, %d opens", ents, err, opens)
	}
	r.Close()

	f, err := os.Open(handed)
	if err != nil {
		t.Fatal(err)
	}
	opens = 0
	r = NewTranscriptReaderFrom(missing, f, open, ReaderOpts{})
	defer r.Close() //nolint:errcheck
	ents, err = r.Read(0, 0)
	if err != nil || len(ents) != 1 || ents[0].Summary != "from the handed fd" || opens != 0 {
		t.Errorf("handed fd: %+v, %v, %d opens", ents, err, opens)
	}

	// A rotation the probe notices is reopened through open as well.
	opens = 0
	r = NewTranscriptReaderFrom(elsewhere, nil, open, ReaderOpts{})
	defer r.Close() //nolint:errcheck
	if _, err := r.Tail(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(elsewhere, []byte(line("rotated")), 0o600); err != nil {
		t.Fatal(err)
	}
	ents, err = r.Tail()
	if err != nil || len(ents) != 1 || ents[0].Summary != "rotated" || opens != 2 {
		t.Errorf("rotation: %+v, %v, %d opens; want the new file through a second open", ents, err, opens)
	}
}

func TestReadFirstLineIDs(t *testing.T) {
	t.Parallel()
	f, err := os.Open(probeAgentFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// The probe's first line has sessionId after message.
	sid, aid, err := ReadFirstLineIDs(f)
	if err != nil || sid != "04a8fc10-6fa5-4b8e-82ba-621974425917" || aid != "a2093755b9a9ce8c0" {
		t.Errorf("probe: %q %q %v", sid, aid, err)
	}

	big := `{"message":{"content":"` + strings.Repeat("x", 40<<10) + `"},"agentId":"abc","sessionId":"s1"}` + "\n{}\n"
	if sid, aid, err := ReadFirstLineIDs(strings.NewReader(big)); err != nil || sid != "s1" || aid != "abc" {
		t.Errorf("40KiB first line: %q %q %v", sid, aid, err)
	}
	if _, _, err := ReadFirstLineIDs(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Errorf("empty: %v, want io.EOF", err)
	}
	if _, _, err := ReadFirstLineIDs(strings.NewReader(`{"sessionId":"s1","mess`)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("half a line: %v, want io.ErrUnexpectedEOF", err)
	}
}
