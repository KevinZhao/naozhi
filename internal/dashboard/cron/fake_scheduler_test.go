package cron

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// fakeScheduler is a SchedulerView with no *cronpkg.Scheduler behind it. It
// exists to make the #2561 promise ("tests need not build the real object")
// true for at least one behavioural test per interface (#2635): before this
// file every dashcron test went through cronpkg.NewScheduler(AllowNilRouter).
//
// Only the methods HandleList / HandlePause exercise have real bodies; the
// rest return zero values so the compile-time interface check below is the
// only thing that has to change when SchedulerView grows.
type fakeScheduler struct {
	jobs      []cronpkg.JobWithNextRun
	paused    []string
	pauseErr  error
	startedAt time.Time
	loc       *time.Location
}

var _ SchedulerView = (*fakeScheduler)(nil)

func (f *fakeScheduler) AddJob(*cronpkg.Job) error             { return nil }
func (f *fakeScheduler) GetJob(string) (cronpkg.Job, bool)     { return cronpkg.Job{}, false }
func (f *fakeScheduler) ListJobs(string, string) []cronpkg.Job { return nil }
func (f *fakeScheduler) ListAllJobsWithNextRun() []cronpkg.JobWithNextRun {
	return f.jobs
}
func (f *fakeScheduler) UpdateJob(string, cronpkg.JobUpdate) (*cronpkg.Job, error) { return nil, nil }
func (f *fakeScheduler) DeleteJobByID(string) (*cronpkg.Job, error)                { return nil, nil }
func (f *fakeScheduler) PauseJobByID(id string) (*cronpkg.Job, error) {
	if f.pauseErr != nil {
		return nil, f.pauseErr
	}
	f.paused = append(f.paused, id)
	return &cronpkg.Job{ID: id, Paused: true}, nil
}
func (f *fakeScheduler) ResumeJobByID(string) (*cronpkg.Job, error) { return nil, nil }
func (f *fakeScheduler) TriggerNow(string) error                    { return nil }
func (f *fakeScheduler) Run(string, string) (*cronpkg.CronRun, error) {
	return nil, cronpkg.ErrJobNotFound
}
func (f *fakeScheduler) ListRuns(string, int, time.Time) []cronpkg.CronRunSummary { return nil }
func (f *fakeScheduler) RecentRuns(string, int) []cronpkg.CronRunSummary          { return nil }
func (f *fakeScheduler) CurrentRun(string) (cronpkg.RunInflightView, bool) {
	return cronpkg.RunInflightView{}, false
}
func (f *fakeScheduler) ListSandboxAttention() []cronpkg.SandboxAttentionItem { return nil }
func (f *fakeScheduler) ConfirmSandboxRun(string) error                       { return nil }
func (f *fakeScheduler) ReplaySandboxRun(string, string) (string, error)      { return "", nil }
func (f *fakeScheduler) SandboxRunEvents(string, string, int) ([][]byte, bool, error) {
	return nil, false, nil
}
func (f *fakeScheduler) SandboxRunSnapshotManifest(string, string) (*cronpkg.SandboxRunSnapshot, bool, error) {
	return nil, false, nil
}
func (f *fakeScheduler) SandboxRunSnapshotPrompt(string) (string, error)   { return "", nil }
func (f *fakeScheduler) PreviewScheduleN(string, int) ([]time.Time, error) { return nil, nil }
func (f *fakeScheduler) Location() *time.Location {
	if f.loc == nil {
		return time.UTC
	}
	return f.loc
}
func (f *fakeScheduler) NotifyDefault() cronpkg.NotifyTarget { return cronpkg.NotifyTarget{} }
func (f *fakeScheduler) StartedAt() time.Time                { return f.startedAt }

// TestHandleList_DrivenByFakeScheduler: the list handler renders whatever the
// view reports, without a real scheduler, store, or router behind it.
func TestHandleList_DrivenByFakeScheduler(t *testing.T) {
	t.Parallel()
	now := time.Now()
	fs := &fakeScheduler{
		startedAt: now.Add(-time.Hour),
		jobs: []cronpkg.JobWithNextRun{
			{Job: cronpkg.Job{ID: strings.Repeat("a", 16), Schedule: "@every 1h", Prompt: "hello", Title: "first", CreatedAt: now}, NextRun: now.Add(time.Hour)},
			{Job: cronpkg.Job{ID: strings.Repeat("b", 16), Schedule: "@every 2h", Prompt: "world", Paused: true, CreatedAt: now}},
		},
	}
	h := New(Deps{Scheduler: fs})

	req := httptest.NewRequest(http.MethodGet, "/api/cron", nil)
	w := httptest.NewRecorder()
	h.HandleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Jobs []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Paused bool   `json:"paused"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	if len(resp.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2: %s", len(resp.Jobs), w.Body.String())
	}
	if resp.Jobs[0].ID != fs.jobs[0].Job.ID || resp.Jobs[0].Title != "first" {
		t.Errorf("job[0] = %+v, want id %s title first", resp.Jobs[0], fs.jobs[0].Job.ID)
	}
	if !resp.Jobs[1].Paused {
		t.Errorf("job[1] paused flag not propagated from the view: %+v", resp.Jobs[1])
	}
}

// TestHandlePause_DrivenByFakeScheduler: the write path forwards the validated
// id to the view and maps its sentinel errors to HTTP codes — asserted against
// a fake so the mapping, not the real scheduler's state machine, is under test.
func TestHandlePause_DrivenByFakeScheduler(t *testing.T) {
	t.Parallel()
	id := strings.Repeat("c", 16)
	pause := func(fs *fakeScheduler) *httptest.ResponseRecorder {
		h := New(Deps{Scheduler: fs})
		req := httptest.NewRequest(http.MethodPost, "/api/cron/pause", strings.NewReader(`{"id":"`+id+`"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.HandlePause(w, req)
		return w
	}

	fs := &fakeScheduler{}
	if w := pause(fs); w.Code != http.StatusOK {
		t.Fatalf("happy path status = %d, body %s", w.Code, w.Body.String())
	}
	if len(fs.paused) != 1 || fs.paused[0] != id {
		t.Fatalf("view received PauseJobByID(%v), want [%s]", fs.paused, id)
	}

	for _, tc := range []struct {
		err  error
		want int
	}{
		{cronpkg.ErrJobNotFound, http.StatusNotFound},
		{cronpkg.ErrJobAlreadyPaused, http.StatusConflict},
		{errors.New("something else"), cronpkg.ClassifyError(errors.New("something else")).HTTPStatus()},
	} {
		if w := pause(&fakeScheduler{pauseErr: tc.err}); w.Code != tc.want {
			t.Errorf("PauseJobByID error %v: status = %d, want %d (body %s)", tc.err, w.Code, tc.want, w.Body.String())
		}
	}
}
