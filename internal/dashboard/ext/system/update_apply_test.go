package system

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/selfupdate"
	"github.com/naozhi/naozhi/internal/session"
)

// applyTestHandlers builds Handlers whose apply path is stubbed, so these tests
// exercise the HTTP contract without any chance of a real install (or a GitHub
// request) escaping a unit test. The returned func reports how many times the
// apply actually ran and with what `restart` argument.
func applyTestHandlers(t *testing.T, fixture selfupdate.StatusFixture) (*Handlers, func() (int, bool)) {
	t.Helper()
	// The preflight verdict is cached process-wide and keyed on nothing, so a
	// neighbouring case's version could otherwise decide this one's outcome.
	selfupdate.InvalidatePreflightCache()
	t.Cleanup(selfupdate.InvalidatePreflightCache)

	router := session.NewRouter(session.RouterConfig{
		MaxProcs:  2,
		Workspace: t.TempDir(),
	})
	// A real Checker so the handler's "auto-update disabled" branch is not the
	// one under test. POST never reaches it (applyFn wins), but GET DOES:
	// HandleUpdateStatus calls CheckNow whenever Status.Latest is empty, and a
	// fixture without Latest would send this unit test to GitHub. CurrentVersion
	// is pinned to "dev" so CheckNow refuses before the network (ErrCheckSkippedDev)
	// — the handler never reads the Checker's version, only the Status's, so the
	// fixtures keep meaning what they say.
	checker := selfupdate.NewChecker(selfupdate.CheckerConfig{
		CurrentVersion: "dev",
		Interval:       time.Hour,
	})
	h := New(Deps{
		Router:         router,
		BuildVersion:   fixture.Current,
		UpdateStatus:   selfupdate.NewStatusFixture(fixture),
		UpdateChecker:  checker,
		InstallEnabled: true,
	})

	var mu sync.Mutex
	var calls int
	var lastRestart bool
	done := make(chan struct{}, 8)
	h.applyFn = func(_ context.Context, restart bool) error {
		mu.Lock()
		calls++
		lastRestart = restart
		mu.Unlock()
		done <- struct{}{}
		return nil
	}
	return h, func() (int, bool) {
		// The apply is detached, so give the goroutine a bounded moment to land
		// rather than reading the counter immediately.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		mu.Lock()
		defer mu.Unlock()
		return calls, lastRestart
	}
}

func postApply(t *testing.T, h *Handlers, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/system/update/apply", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleUpdateApply(w, r)
	return w
}

// installFixture is a state where an install is genuinely applicable: a newer
// release exists, nothing is staged. The restart-only state cannot be used for
// the happy path because its preflight requires a managed service, which a test
// process does not have.
func installFixture() selfupdate.StatusFixture {
	return selfupdate.StatusFixture{
		Current: "v1.0.0", Latest: "v1.1.0", Phase: selfupdate.PhaseAvailable,
	}
}

func TestUpdateApply_Accepted(t *testing.T) {
	h, applied := applyTestHandlers(t, installFixture())

	w := postApply(t, h, `{"confirm_action":"install"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "started") {
		t.Errorf("body = %q, want a started status", w.Body.String())
	}
	calls, _ := applied()
	if calls != 1 {
		t.Errorf("apply ran %d times, want 1", calls)
	}
}

// The 202 must come back before the work finishes. On the real path the process
// is SIGTERMed by the service manager partway through, so a synchronous handler
// would never get its response out — the browser would see a dropped connection
// and no way to tell success from failure.
func TestUpdateApply_RespondsBeforeWorkCompletes(t *testing.T) {
	h, _ := applyTestHandlers(t, installFixture())
	release := make(chan struct{})
	entered := make(chan struct{})
	h.applyFn = func(context.Context, bool) error {
		close(entered)
		<-release
		return nil
	}

	w := postApply(t, h, `{"confirm_action":"install"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 while the apply is still running", w.Code)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("apply goroutine never started")
	}
	close(release)
}

// The background work must run on a context that outlives the request and still
// has a deadline. Both halves matter: r.Context() is cancelled the moment the
// 202 is written (killing a download a fraction of a second after it starts),
// while a bare Background() would let a wedged transfer pin the goroutine for
// good.
//
// The live assertion can only check the context the apply actually receives —
// under httptest the request context is never cancelled, so it cannot prove
// where the context came from. The source guard below covers that half.
func TestUpdateApply_BackgroundContextSurvivesRequest(t *testing.T) {
	h, _ := applyTestHandlers(t, installFixture())
	type probe struct {
		err         error
		hasDeadline bool
	}
	got := make(chan probe, 1)
	h.applyFn = func(ctx context.Context, _ bool) error {
		p := probe{err: ctx.Err()}
		_, p.hasDeadline = ctx.Deadline()
		got <- p
		return nil
	}

	if w := postApply(t, h, `{"confirm_action":"install"}`); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	select {
	case p := <-got:
		if p.err != nil {
			t.Errorf("apply context already cancelled (%v)", p.err)
		}
		if !p.hasDeadline {
			t.Error("apply context has no deadline; a wedged download would pin the goroutine forever")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("apply goroutine never started")
	}
}

// Source guard for the half the runtime test cannot see: the detached goroutine
// must build its own context from Background(), never from the request.
func TestUpdateApply_DetachedContextSourceGuard(t *testing.T) {
	src := readUpdateSource(t, false)
	at := strings.Index(src, "func (h *Handlers) HandleUpdateApply")
	if at < 0 {
		t.Fatal("HandleUpdateApply not found")
	}
	goAt := strings.Index(src[at:], "go func()")
	if goAt < 0 {
		t.Fatal("HandleUpdateApply must do its work in a detached goroutine (the process is killed mid-restart, so a synchronous response never ships)")
	}
	// Strip comment lines: the code deliberately EXPLAINS why r.Context() is
	// wrong, and a naive scan would read that explanation as the violation.
	var code []string
	for _, line := range strings.Split(src[at+goAt:], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	tail := strings.Join(code, "\n")
	if !strings.Contains(tail, "context.WithTimeout(context.Background()") {
		t.Error("the apply goroutine must use context.WithTimeout(context.Background(), …): a request-derived context is cancelled as soon as the 202 is written")
	}
	if strings.Contains(tail, "r.Context()") {
		t.Error("the apply goroutine must NOT use r.Context() — the request is already over by the time it runs")
	}
}

// flushRecorder records whether the handler flushed its response, and when
// relative to other events.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func (f *flushRecorder) Flush() { f.once.Do(func() { close(f.flushed) }) }

// TestUpdateApply_FlushesAcceptedBeforeStartingWork pins the ordering that keeps
// the operator from being told an apply failed while it succeeds.
//
// On the restart path the goroutine reaches `launchctl kickstart -k` /
// `systemctl restart` within milliseconds — there is no download in front of it
// — and the SIGTERM that follows can kill this process before net/http gets
// around to shipping the response, because a handler's bytes sit in a buffer
// until it returns. The browser then sees a dropped connection, which
// applyUpdate() reports as "升级请求失败" on an apply that is in fact working.
// So the 202 must be written AND flushed before the work is started.
func TestUpdateApply_FlushesAcceptedBeforeStartingWork(t *testing.T) {
	h, _ := applyTestHandlers(t, installFixture())
	fr := &flushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	// Reported through a channel rather than a plain variable: the apply runs on
	// another goroutine and -race would (rightly) flag a shared bool.
	sawFlush := make(chan bool, 1)
	h.applyFn = func(context.Context, bool) error {
		select {
		case <-fr.flushed:
			sawFlush <- true
		default:
			sawFlush <- false
		}
		return nil
	}

	r := httptest.NewRequest(http.MethodPost, "/api/system/update/apply",
		strings.NewReader(`{"confirm_action":"install"}`))
	r.Header.Set("Content-Type", "application/json")
	h.HandleUpdateApply(fr, r)

	if fr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%q", fr.Code, fr.Body.String())
	}
	select {
	case <-fr.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never flushed the 202: net/http would hold it in a buffer until the handler returns, and on the restart path this process can be SIGTERMed first")
	}
	select {
	case ok := <-sawFlush:
		if !ok {
			t.Error("the apply started before the 202 was flushed — a restart racing the response makes the dashboard report failure for an apply that is succeeding")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("apply goroutine never started")
	}
}

// TestUpdateApply_FlushOrderSourceGuard covers what the live test cannot: under
// httptest the flush always wins by a wide margin, so a future edit that moved
// the write back below `go func()` would keep passing there. The ordering is the
// invariant, so the ordering is what gets pinned.
func TestUpdateApply_FlushOrderSourceGuard(t *testing.T) {
	body := funcBody(t, readUpdateSource(t, true), "HandleUpdateApply")

	writeAt := strings.Index(body, "httputil.WriteJSONStatus(w, http.StatusAccepted")
	flushAt := strings.Index(body, "f.Flush()")
	goAt := strings.Index(body, "go func()")
	if writeAt < 0 || goAt < 0 {
		t.Fatal("expected HandleUpdateApply to write a 202 and detach the work")
	}
	if flushAt < 0 {
		t.Fatal("HandleUpdateApply must Flush the 202: writing it is not shipping it, and the restart can kill this process before the handler returns")
	}
	if writeAt > goAt || flushAt > goAt {
		t.Error("the 202 must be written AND flushed BEFORE the apply goroutine starts — otherwise a restart-only apply can SIGTERM this process while the response is still buffered")
	}
}

// TestUpdateApply_ConfirmActionMismatch is the TOCTOU gate: the operator agreed
// to the operation they were SHOWN. If the background checker changed the
// situation in between, the click must be refused rather than silently mean
// something else — in the worst case, a second install over a staged binary.
func TestUpdateApply_ConfirmActionMismatch(t *testing.T) {
	h, applied := applyTestHandlers(t, installFixture())

	w := postApply(t, h, `{"confirm_action":"restart"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 on a stale confirm_action; body=%q", w.Code, w.Body.String())
	}
	if calls, _ := applied(); calls != 0 {
		t.Errorf("apply ran %d times on a mismatched confirmation, want 0", calls)
	}
}

func TestUpdateApply_NothingToDo(t *testing.T) {
	h, applied := applyTestHandlers(t, selfupdate.StatusFixture{Current: "v1.0.0"})

	w := postApply(t, h, `{"confirm_action":"install"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 when there is nothing to apply", w.Code)
	}
	if calls, _ := applied(); calls != 0 {
		t.Errorf("apply ran %d times with nothing to do, want 0", calls)
	}
}

// A blocked preflight must fail fast AND start nothing. The restart-only state
// with no managed service is the real-world case: the binary is staged but only
// a human can restart the process.
func TestUpdateApply_BlockedPreflightStartsNothing(t *testing.T) {
	h, applied := applyTestHandlers(t, selfupdate.StatusFixture{
		Current: "v1.0.0", Latest: "v1.1.0", Staged: "v1.1.0",
		Phase: selfupdate.PhaseStaged,
	})

	w := postApply(t, h, `{"confirm_action":"restart"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 when preflight blocks; body=%q", w.Code, w.Body.String())
	}
	if calls, _ := applied(); calls != 0 {
		t.Errorf("apply ran %d times behind a blocked preflight, want 0", calls)
	}
}

// update.dashboard_install=false: the read-only endpoint keeps working, the
// apply endpoint does not.
func TestUpdateApply_DisabledByConfig(t *testing.T) {
	selfupdate.InvalidatePreflightCache()
	t.Cleanup(selfupdate.InvalidatePreflightCache)
	router := session.NewRouter(session.RouterConfig{MaxProcs: 2, Workspace: t.TempDir()})
	h := New(Deps{
		Router:         router,
		BuildVersion:   "v1.0.0",
		UpdateStatus:   selfupdate.NewStatusFixture(installFixture()),
		InstallEnabled: false,
	})
	h.applyFn = func(context.Context, bool) error {
		t.Error("apply must not run when update.dashboard_install is false")
		return nil
	}

	if w := postApply(t, h, `{"confirm_action":"install"}`); w.Code != http.StatusForbidden {
		t.Errorf("apply status = %d, want 403", w.Code)
	}
	// GET is unaffected: an operator who cannot click still needs to know.
	code, body := getUpdateStatus(t, h)
	if code != http.StatusOK {
		t.Fatalf("status endpoint = %d, want 200 with installs disabled", code)
	}
	if got := body["install_enabled"]; got != false {
		t.Errorf("install_enabled = %v, want false so the UI shows the manual command", got)
	}
	if got := body["action"]; got != "install" {
		t.Errorf("action = %v, want install; the status must stay truthful", got)
	}
}

// No checker at all (update.enabled=false) means there is no subsystem to run
// an install, even though the version chip still works.
func TestUpdateApply_NoCheckerConflicts(t *testing.T) {
	selfupdate.InvalidatePreflightCache()
	t.Cleanup(selfupdate.InvalidatePreflightCache)
	router := session.NewRouter(session.RouterConfig{MaxProcs: 2, Workspace: t.TempDir()})
	h := New(Deps{
		Router:       router,
		BuildVersion: "v1.0.0",
		UpdateStatus: selfupdate.NewStatusFixture(installFixture()),
		// UpdateChecker nil.
		InstallEnabled: true,
	})
	if w := postApply(t, h, `{"confirm_action":"install"}`); w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 with no checker wired", w.Code)
	}
	_, body := getUpdateStatus(t, h)
	if got := body["install_enabled"]; got != false {
		t.Errorf("install_enabled = %v, want false when no checker can perform the install", got)
	}
}

// Rate limit: a held-down button must not spray the failure path (log lines and,
// on the install branch, GitHub requests). Rejected attempts consume the window
// too — the aim is to bound attempts, not successes.
func TestUpdateApply_RateLimited(t *testing.T) {
	h, _ := applyTestHandlers(t, selfupdate.StatusFixture{Current: "v1.0.0"})

	if w := postApply(t, h, `{"confirm_action":"install"}`); w.Code != http.StatusConflict {
		t.Fatalf("first attempt status = %d, want 409 (nothing to do)", w.Code)
	}
	if w := postApply(t, h, `{"confirm_action":"install"}`); w.Code != http.StatusTooManyRequests {
		t.Errorf("second attempt status = %d, want 429", w.Code)
	}
}

func TestUpdateApply_InvalidBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"not json", "nope"},
		{"unknown field", `{"confirm_action":"install","also":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, applied := applyTestHandlers(t, installFixture())
			if w := postApply(t, h, tc.body); w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%q", w.Code, w.Body.String())
			}
			if calls, _ := applied(); calls != 0 {
				t.Errorf("apply ran %d times on an invalid body, want 0", calls)
			}
		})
	}
}

// TestUpdateStatusRollbackHint: the recovery command must be in the payload
// BEFORE anything is applied. If the new build does not boot, the dashboard that
// would have carried this advice is exactly what is gone.
func TestUpdateStatusRollbackHint(t *testing.T) {
	selfupdate.InvalidatePreflightCache()
	t.Cleanup(selfupdate.InvalidatePreflightCache)
	h := newUpdateHandlers(t, selfupdate.NewStatusFixture(installFixture()), "v1.0.0")

	_, body := getUpdateStatus(t, h)
	hint, _ := body["rollback_hint"].(string)
	if hint == "" {
		t.Fatal("rollback_hint empty while an update is actionable")
	}
	if !strings.Contains(hint, ".naozhi-upgrade.bak") {
		t.Errorf("rollback_hint = %q, want it to reference the backup file", hint)
	}

	// Nothing to apply ⇒ no hint to give.
	h2 := newUpdateHandlers(t, selfupdate.NewStatusFixture(selfupdate.StatusFixture{Current: "v1.0.0"}), "v1.0.0")
	_, body2 := getUpdateStatus(t, h2)
	if _, present := body2["rollback_hint"]; present {
		t.Errorf("rollback_hint present with nothing to apply: %v", body2["rollback_hint"])
	}
}

// manual_command is server-computed. install ⇒ `naozhi upgrade`; a staged
// binary with no service managing this process ⇒ "" (blocked_reason already
// tells the operator to restart the process by hand, and a guessed `systemctl
// restart naozhi` would hit the wrong service). The managed-restart variant
// lives in selfupdate.TestManualCommand where the probe can be stubbed.
func TestUpdateStatusManualCommand(t *testing.T) {
	h, _ := applyTestHandlers(t, installFixture())
	_, body := getUpdateStatus(t, h)
	if got := body["manual_command"]; got != "naozhi upgrade" {
		t.Errorf("manual_command = %v for action=install, want naozhi upgrade", got)
	}

	h2, _ := applyTestHandlers(t, selfupdate.StatusFixture{
		Current: "v1.0.0", Latest: "v1.1.0", Staged: "v1.1.0", Phase: selfupdate.PhaseStaged,
	})
	_, body2 := getUpdateStatus(t, h2)
	if got, present := body2["manual_command"]; !present {
		t.Error("manual_command must always be present (empty when there is nothing to paste)")
	} else if body2["restart_supported"] == false && got != "" {
		t.Errorf("manual_command = %v with no service managing this process; must be empty rather than restart something else", got)
	}
}

// A panic inside the detached apply must neither crash the process nor leave
// the status parked on a busy phase with nothing to show.
func TestUpdateApply_PanicIsRecovered(t *testing.T) {
	h, _ := applyTestHandlers(t, installFixture())
	entered := make(chan struct{})
	h.applyFn = func(context.Context, bool) error {
		defer close(entered)
		panic("boom")
	}
	w := postApply(t, h, `{"confirm_action":"install"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%q", w.Code, w.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("apply goroutine never ran")
	}
	// `entered` closes inside the panicking seam, BEFORE the handler goroutine's
	// deferred recover runs MarkFailed; the production goroutine exposes no
	// signal after that point, so this is a bounded Eventually (2s, 10ms poll),
	// not a deterministic join. It only ever waits for a defer that is already
	// unwinding, so in practice it completes on the first or second poll.
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := h.updateStatus.Snapshot()
		if snap.Phase == selfupdate.PhaseFailed {
			if snap.LastErr == "" {
				t.Error("last_error must carry the panic so the operator sees why")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %q after a panicking apply, want %q", snap.Phase, selfupdate.PhaseFailed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
