package agentevents

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

const (
	wfSID   = "04a8fc10-6fa5-4b8e-82ba-621974425917"
	wfAgent = "a2093755b9a9ce8c0"
)

// probeTranscript is the probe's workflow agent transcript (framed prompt,
// then the answer "4").
func probeTranscript(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "subagent", "testdata", "agent-"+wfAgent+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// wfRig is a handler whose board lookup finds wfAgent's transcript in a run
// dir under a temp projects root, with no linker at all; opens counts the
// transcript's opens.
type wfRig struct {
	h      *Handler
	root   string
	path   string
	tr     session.AgentTranscript
	status session.TranscriptStatus
	opens  int
}

func newWFRig(t *testing.T, data []byte) *wfRig {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &wfRig{root: filepath.Join(base, "projects"), status: session.TranscriptReady}
	dir := filepath.Join(r.root, "-ws", wfSID, "subagents", "workflows", "wf_2997921d-435")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r.path = filepath.Join(dir, "agent-"+wfAgent+".jsonl")
	if data != nil {
		if err := os.WriteFile(r.path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.tr = session.AgentTranscript{Path: r.path, RunSessionID: wfSID, Open: func() (*os.File, error) {
		r.opens++
		f, _, err := osutil.OpenRegular(r.path, 0)
		return f, err
	}}
	r.h = &Handler{
		allowedRoot: r.root,
		linkerFor:   func(string) agentlink.AgentLinker { return nil },
		workflowFor: func(_, id string) (session.AgentTranscript, session.TranscriptStatus) {
			if id != wfAgent {
				return session.AgentTranscript{}, session.TranscriptNone
			}
			return r.tr, r.status
		},
	}
	return r
}

func (r *wfRig) get(t *testing.T, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.h.HandleAgentEvents(rec, agentEventsReq(testAgentEventsKey, taskID, "", ""))
	return rec
}

func decodeEntries(t *testing.T, rec *httptest.ResponseRecorder) []clievent.EventEntry {
	t.Helper()
	var out []clievent.EventEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// TestAgentEvents_WorkflowAgentBeforeLinker: the board answers before the
// linker nil check, so a session without a process serves its workflow
// agent; the first entry is the task, not the harness frame; an id the
// board does not know still takes the linker path (404 with no linker).
func TestAgentEvents_WorkflowAgentBeforeLinker(t *testing.T) {
	t.Parallel()
	r := newWFRig(t, probeTranscript(t))
	rec := r.get(t, wfAgent)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	ents := decodeEntries(t, rec)
	if len(ents) != 2 || ents[0].Detail != "Reply with just the number 2+2" || ents[1].Detail != "4" {
		t.Errorf("entries %+v", ents)
	}
	if rec := r.get(t, "a0ba344a06862740f"); rec.Code != http.StatusNotFound {
		t.Errorf("an id the board does not know: %d, want the linker path's 404", rec.Code)
	}
}

// TestAgentEvents_WorkflowAgentPending: an unresolved run dir, a missing
// transcript, an empty one and a half-written first line are all 202.
func TestAgentEvents_WorkflowAgentPending(t *testing.T) {
	t.Parallel()
	r := newWFRig(t, nil)
	r.status = session.TranscriptPending
	if rec := r.get(t, wfAgent); rec.Code != http.StatusAccepted || r.opens != 0 {
		t.Errorf("run dir unresolved: %d after %d opens, want 202 and none", rec.Code, r.opens)
	}
	r.status = session.TranscriptReady
	data := probeTranscript(t)
	for name, body := range map[string][]byte{"missing": nil, "empty": {}, "half a first line": data[:200]} {
		if body != nil {
			if err := os.WriteFile(r.path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if rec := r.get(t, wfAgent); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "pending") {
			t.Errorf("%s: %d %q, want 202 pending", name, rec.Code, rec.Body.String())
		}
	}
}

// TestAgentEvents_WorkflowAgentFirstLine: the first line must name the run
// dir's session and the agent asked for; a first line over 32KiB is read.
func TestAgentEvents_WorkflowAgentFirstLine(t *testing.T) {
	t.Parallel()
	data := string(probeTranscript(t))
	cases := []struct {
		name, body string
		want       int
	}{
		{"another session", strings.Replace(data, wfSID, "11111111-1111-4111-8111-111111111111", 1), http.StatusNotFound},
		{"another agent", strings.Replace(data, `"agentId":"`+wfAgent, `"agentId":"a0ba344a06862740f`, 1), http.StatusNotFound},
		{"not JSON", "not json\n", http.StatusNotFound},
		{"first line over 32KiB", strings.Replace(data, "2+2", "2+2"+strings.Repeat(" ", 40<<10), 1), http.StatusOK},
	}
	for _, c := range cases {
		r := newWFRig(t, []byte(c.body))
		if rec := r.get(t, wfAgent); rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

// TestAgentEvents_WorkflowAgentOutsideRoot: a board path outside the
// handler's root is refused before anything opens it.
func TestAgentEvents_WorkflowAgentOutsideRoot(t *testing.T) {
	t.Parallel()
	r := newWFRig(t, probeTranscript(t))
	r.h.allowedRoot = filepath.Join(filepath.Dir(r.root), "elsewhere")
	if rec := r.get(t, wfAgent); rec.Code != http.StatusNotFound || r.opens != 0 {
		t.Errorf("%d after %d opens, want 404 and none", rec.Code, r.opens)
	}
}
