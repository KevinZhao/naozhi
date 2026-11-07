package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	dashplanner "github.com/naozhi/naozhi/internal/dashboard/ext/planner"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// plannerFileSessions is the router as the Planner sees it: planner:main
// alive and one more session to hand over.
type plannerFileSessions struct{}

const plannerFileRef = "dashboard:direct:a:general"

func (plannerFileSessions) State(key string) agent.SessionState {
	if key == agent.Key || key == plannerFileRef {
		return agent.SessionState{Exists: true, Alive: true, SessionID: "sid"}
	}
	return agent.SessionState{}
}
func (plannerFileSessions) Ensure(context.Context, string, sessionview.AgentOpts) error   { return nil }
func (plannerFileSessions) Recreate(context.Context, string, sessionview.AgentOpts) error { return nil }
func (plannerFileSessions) Release(string) bool                                           { return true }
func (plannerFileSessions) Label(string) string                                           { return "" }
func (plannerFileSessions) Name(string) string                                            { return "" }

// plannerFileTurns records what reaches the Planner's turn.
type plannerFileTurns struct {
	mu     sync.Mutex
	texts  []string
	images [][]clievent.Attachment
}

func (f *plannerFileTurns) SubmitPlanner(_ string, _ agent.Source, _ *agent.Words, text string, images []clievent.Attachment, _ sessionview.AgentOpts, _ func(string, error)) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	f.images = append(f.images, images)
	return false, nil
}
func (*plannerFileTurns) PlannerTurnSources(string) []agent.Source { return nil }
func (*plannerFileTurns) PlannerTurnWords(string) []agent.Words    { return nil }
func (*plannerFileTurns) PlannerReceipt(string, string)            {}
func (*plannerFileTurns) PlannerNote(string, string)               {}
func (*plannerFileTurns) DropPlannerQueue(string)                  {}

func (f *plannerFileTurns) sent() ([]string, [][]clievent.Attachment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...), append([][]clievent.Attachment(nil), f.images...)
}

// TestPlannerSendFileIDs: POST /api/planner/send resolves file_ids as a
// session send does — the uploader's own live uploads reach the Planner's
// turn (also with no text), an expired, unknown or someone else's file_id is
// refused before anything is consumed, and a refusal after the take says
// files_consumed.
func TestPlannerSendFileIDs(t *testing.T) {
	const token = "planner-files-token"
	owner := bearerOwner(token)
	png := clievent.Attachment{Kind: clievent.KindImageInline, MimeType: "image/png", Data: []byte("\x89PNG-fixture"), OrigName: "a.png"}

	for _, tc := range []struct {
		name string
		// body gets the seeded upload's ID for "$fid".
		body     map[string]any
		seedAs   string // the uploader; "" seeds nothing
		expire   bool
		stopped  bool
		status   int
		errText  string
		consumed bool
		// images is how many reach the turn; -1 is no turn at all.
		images   int
		kept     bool // the upload is still in the store for its owner
		textTail string
	}{
		{name: "valid", body: map[string]any{"text": "看这张图", "file_ids": []string{"$fid"}}, seedAs: owner,
			status: http.StatusAccepted, images: 1, textTail: "</naozhi-snapshot>\n\n看这张图"},
		{name: "image only", body: map[string]any{"file_ids": []string{"$fid"}}, seedAs: owner,
			status: http.StatusAccepted, images: 1, textTail: "</naozhi-snapshot>"},
		{name: "handed over with an image", body: map[string]any{"text": "这里", "ref": plannerFileRef, "file_ids": []string{"$fid"}}, seedAs: owner,
			status: http.StatusAccepted, images: 1, textTail: "</naozhi-snapshot>\n\n这里"},
		{name: "expired", body: map[string]any{"text": "x", "file_ids": []string{"$fid"}}, seedAs: owner, expire: true,
			status: http.StatusBadRequest, errText: "file not found or expired", images: -1},
		{name: "unknown", body: map[string]any{"text": "x", "file_ids": []string{"0123456789abcdef0123456789abcdef"}},
			status: http.StatusBadRequest, errText: "file not found or expired", images: -1},
		{name: "someone else's", body: map[string]any{"text": "x", "file_ids": []string{"$fid"}}, seedAs: bearerOwner("other-browser"),
			status: http.StatusBadRequest, errText: "file not found or expired", images: -1, kept: true},
		{name: "too many", body: map[string]any{"text": "x", "file_ids": make([]string, maxFilesPerSend+1)},
			status: http.StatusBadRequest, errText: errTooManyFiles, images: -1},
		{name: "stopped keeps the upload", body: map[string]any{"text": "x", "file_ids": []string{"$fid"}}, seedAs: owner, stopped: true,
			status: http.StatusConflict, errText: agent.ErrStopped.Error(), images: -1, kept: true},
		{name: "refused after the take", body: map[string]any{"text": "x", "ref": "dashboard:direct:gone:general", "file_ids": []string{"$fid"}}, seedAs: owner,
			status: http.StatusNotFound, errText: agent.ErrNoRef.Error(), consumed: true, images: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lay := datadir.FromRoot(t.TempDir())
			a := agent.New(agent.Config{Enabled: true, Layout: lay, Home: t.TempDir(), Sessions: plannerFileSessions{}})
			turns := &plannerFileTurns{}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			a.Start(ctx, turns, nil)
			if tc.stopped {
				if err := a.SetStopped(true); err != nil {
					t.Fatal(err)
				}
			}
			store := newUploadStore()
			fid := ""
			if tc.seedAs != "" {
				var err error
				if fid, err = store.Put(tc.seedAs, png); err != nil {
					t.Fatal(err)
				}
				if tc.expire {
					store.entries[fid].Created = store.entries[fid].Created.Add(-2 * uploadTTL)
				}
			}
			if ids, ok := tc.body["file_ids"].([]string); ok && len(ids) == 1 && ids[0] == "$fid" {
				tc.body["file_ids"] = []string{fid}
			}
			ws, stateDir := t.TempDir(), t.TempDir()
			h := dashplanner.New(a, nil, nil, plannerFiles{store: store, workspace: func() string { return ws }, imageDir: plannerImageDir(stateDir)})

			raw, _ := json.Marshal(tc.body)
			r := httptest.NewRequest(http.MethodPost, "/api/planner/send", strings.NewReader(string(raw)))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.HandleSend(w, r)

			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			var body struct {
				Error         string `json:"error"`
				FilesConsumed bool   `json:"files_consumed"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.Error != tc.errText || body.FilesConsumed != tc.consumed {
				t.Errorf("body %s, want error %q files_consumed %v", w.Body.String(), tc.errText, tc.consumed)
			}
			texts, images := turns.sent()
			if tc.images < 0 {
				if len(texts) != 0 {
					t.Errorf("a refused send reached the turn: %q", texts)
				}
			} else {
				if len(images) != 1 || len(images[0]) != tc.images {
					t.Fatalf("turn images %v, want one turn with %d", images, tc.images)
				}
				if got := images[0][0]; got.MimeType != png.MimeType || string(got.Data) != string(png.Data) {
					t.Errorf("image reached the turn as %s %q", got.MimeType, got.Data)
				}
				if !strings.HasSuffix(texts[0], tc.textTail) {
					t.Errorf("turn text %q, want … %q", texts[0], tc.textTail)
				}
				// The original is kept under the state root, where the
				// attachment route serves the Planner's images from.
				rel := images[0][0].WorkspacePath
				if !strings.HasPrefix(rel, toolImageDirPrefix) {
					t.Fatalf("image path %q, want one under %s", rel, toolImageDirPrefix)
				}
				if data, err := os.ReadFile(filepath.Join(stateDir, filepath.FromSlash(rel))); err != nil || string(data) != string(png.Data) {
					t.Errorf("kept original %q: %v", data, err)
				}
			}
			// A refusal after the take leaves no original behind.
			if kept, _ := filepath.Glob(filepath.Join(stateDir, "planner", "attachments", "*", "*.png")); tc.images < 0 && len(kept) != 0 {
				t.Errorf("a refused send kept %v", kept)
			}
			if fid != "" {
				if left := store.Peek(fid, tc.seedAs) != nil; left != tc.kept {
					t.Errorf("upload left in the store = %v, want %v", left, tc.kept)
				}
			}
		})
	}
}
