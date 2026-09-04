package system

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/selfupdate"
	"github.com/naozhi/naozhi/internal/session"
)

// newUpdateHandlers builds minimal Handlers for exercising
// GET /api/system/update.
func newUpdateHandlers(t *testing.T, status *selfupdate.Status, version string) *Handlers {
	t.Helper()
	router := session.NewRouter(session.RouterConfig{
		MaxProcs:  2,
		Workspace: t.TempDir(),
	})
	return New(Deps{
		Router:       router,
		BuildVersion: version,
		UpdateStatus: status,
		// UpdateChecker deliberately nil: these tests must never reach GitHub.
		InstallEnabled: true,
	})
}

func getUpdateStatus(t *testing.T, h *Handlers) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/system/update", nil)
	w := httptest.NewRecorder()
	h.HandleUpdateStatus(w, req)
	var body map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
		}
	}
	return w.Code, body
}

// readUpdateSource returns update.go with comment lines stripped, so source
// guards match code rather than the prose that explains it.
func readUpdateSource(t *testing.T, stripComments bool) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "update.go"))
	if err != nil {
		t.Fatalf("read update.go: %v", err)
	}
	if !stripComments {
		return string(b)
	}
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

// funcBody returns the source of the named method, bounded at the next
// top-level declaration.
func funcBody(t *testing.T, src, fn string) string {
	t.Helper()
	at := strings.Index(src, "func (h *Handlers) "+fn)
	if at < 0 {
		t.Fatalf("%s not found", fn)
	}
	body := src[at:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	return body
}

// TestUpdateHandlers_ProbeServiceOnce keeps the service probe to one call per
// request in each handler.
//
// It is a source guard because the cost is only visible on darwin, where
// ServiceRunning() forks `launchctl list` — a behavioural test would have to
// stub a probe that is not stubbable there, and on linux the fork does not
// exist to be counted. GET is polled by every open dashboard (every 3s while
// an apply is in flight); whoever needs the verdict takes the caller's copy.
func TestUpdateHandlers_ProbeServiceOnce(t *testing.T) {
	src := readUpdateSource(t, true)
	for _, fn := range []string{"HandleUpdateStatus", "HandleUpdateApply"} {
		body := funcBody(t, src, fn)
		n := strings.Count(body, "selfupdate.ServiceManagesThisProcess()") + strings.Count(body, "selfupdate.ServiceRunning()")
		if n > 1 {
			t.Errorf("%s probes the service %d times; probe once and pass the result to CheckPreflight / RollbackHint / the response field", fn, n)
		}
		// The dashboard restarts THIS process. ServiceRunning() answers "is any
		// naozhi unit active", which from an unmanaged instance beside the
		// system service would restart the wrong one.
		if strings.Contains(body, "selfupdate.ServiceRunning()") {
			t.Errorf("%s must probe selfupdate.ServiceManagesThisProcess(), not ServiceRunning(): the restart has to land on this process", fn)
		}
	}
}

// TestUpdateStatus_ColdStartFillGuard pins the two properties of the on-demand
// fill in HandleUpdateStatus that a behavioural test cannot reach without a
// GitHub stub (the release lookup seam is internal to selfupdate):
//
//   - the gate is `latest == ""` alone. A failed check advances checkedAt, so a
//     gate that also required "never tried" would, after one transient failure
//     at boot, never fill again until the 6h tick.
//   - the check runs under updateColdStartFillTimeout, not CheckNow's own 60s:
//     this is inline in a polled GET.
func TestUpdateStatus_ColdStartFillGuard(t *testing.T) {
	body := funcBody(t, readUpdateSource(t, false), "HandleUpdateStatus")
	if !strings.Contains(body, `latest == "" {`) {
		t.Error(`the cold-start fill must gate on latest == "" only`)
	}
	if strings.Contains(body, "at.IsZero()") {
		t.Error("the cold-start fill must not require checkedAt to be zero: a failed check stamps it, which would disable the fill after one blip")
	}
	if !strings.Contains(body, "updateColdStartFillTimeout") {
		t.Error("CheckNow from the GET handler must run under updateColdStartFillTimeout")
	}
	if updateColdStartFillTimeout > 15*time.Second {
		t.Errorf("updateColdStartFillTimeout = %s; a polled GET must not block for long", updateColdStartFillTimeout)
	}
}

// TestUpdateStatusShape locks the wire contract. dashboard.js branches on
// `action` and reads current/latest/staged/can_apply/blocked_reason/
// running_sessions, so a silently renamed field breaks the version banner in a
// way nobody notices until an upgrade is missed.
func TestUpdateStatusShape(t *testing.T) {
	st := selfupdate.NewStatus("v1.0.0")
	h := newUpdateHandlers(t, st, "v1.0.0")

	code, body := getUpdateStatus(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	// Always present, even with nothing to report — the dashboard polls this
	// and must not have to distinguish "absent" from "nothing to do".
	// install_enabled is load bearing: the chip offers the button only when
	// can_apply AND install_enabled are both true, so if this field ever
	// stopped being emitted the feature would silently degrade to "always show
	// the manual command" with every other test still green.
	//
	// manual_command is what the chip prints when it cannot apply; it is always
	// emitted (empty when there is nothing to paste) so the browser never has to
	// fall back to guessing the server's platform.
	required := []string{
		"current", "latest", "staged", "phase", "action",
		"can_apply", "restart_supported", "running_sessions", "enabled",
		"install_enabled", "manual_command",
	}
	for _, k := range required {
		if _, ok := body[k]; !ok {
			t.Errorf("required field %q missing from response: %v", k, body)
		}
	}

	if got := body["current"]; got != "v1.0.0" {
		t.Errorf("current = %v, want v1.0.0", got)
	}
	if got := body["action"]; got != string(selfupdate.ActionNone) {
		t.Errorf("action = %v, want %q", got, selfupdate.ActionNone)
	}
	// Phase must never be the empty string on the wire.
	if got := body["phase"]; got != string(selfupdate.PhaseIdle) {
		t.Errorf("phase = %v, want %q", got, selfupdate.PhaseIdle)
	}
	// enabled tracks the CHECKER, not the Status: main.go always builds a
	// Status (so `current` is always known), but only update.enabled=true wires
	// a Checker that maintains `latest`. With a Status and no Checker the
	// endpoint must not claim the version information is being kept current.
	if got := body["enabled"]; got != false {
		t.Errorf("enabled = %v, want false when no Checker is wired (Status alone does not keep latest current)", got)
	}
}

// TestUpdateStatusEnabledTracksChecker is the positive half of the `enabled`
// contract. A dev-build Checker is used because CheckNow refuses to reach
// GitHub for dev builds, so the cold-start fill in the handler stays offline.
func TestUpdateStatusEnabledTracksChecker(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{MaxProcs: 2, Workspace: t.TempDir()})
	checker := selfupdate.NewChecker(selfupdate.CheckerConfig{
		CurrentVersion: "dev",
		Interval:       time.Hour,
	})
	h := New(Deps{
		Router:         router,
		BuildVersion:   "dev",
		UpdateStatus:   selfupdate.NewStatus("dev"),
		UpdateChecker:  checker,
		InstallEnabled: true,
	})
	code, body := getUpdateStatus(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := body["enabled"]; got != true {
		t.Errorf("enabled = %v, want true when a Checker is wired", got)
	}
}

// TestUpdateStatusActionMatrix is the server-side half of the guard in
// selfupdate.TestStatusSnapshotAction: the browser must receive the correct
// `action`, because it never compares versions itself.
//
// The staged row is the one that matters. Reporting "install" while a binary is
// already staged leads to a second Replace, which overwrites the backup with
// the new version and destroys the rollback path (RFC §1.3).
func TestUpdateStatusActionMatrix(t *testing.T) {
	tests := []struct {
		name       string
		fixture    selfupdate.StatusFixture
		wantAction string
		wantStaged string
	}{
		{
			name:       "nothing known",
			fixture:    selfupdate.StatusFixture{Current: "v1.0.0"},
			wantAction: "none",
		},
		{
			name: "newer release available",
			fixture: selfupdate.StatusFixture{
				Current: "v1.0.0", Latest: "v1.1.0", Phase: selfupdate.PhaseAvailable,
			},
			wantAction: "install",
		},
		{
			name: "staged awaiting restart",
			fixture: selfupdate.StatusFixture{
				Current: "v1.0.0", Latest: "v1.1.0", Staged: "v1.1.0",
				Phase: selfupdate.PhaseStaged,
			},
			wantAction: "restart",
			wantStaged: "v1.1.0",
		},
		{
			name: "remote is older (downgrade attempt)",
			fixture: selfupdate.StatusFixture{
				Current: "v1.2.0", Latest: "v1.0.0",
			},
			wantAction: "none",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := selfupdate.NewStatusFixture(tc.fixture)
			h := newUpdateHandlers(t, st, tc.fixture.Current)

			code, body := getUpdateStatus(t, h)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if got := body["action"]; got != tc.wantAction {
				t.Errorf("action = %v, want %q", got, tc.wantAction)
			}
			if tc.wantStaged != "" {
				if got := body["staged"]; got != tc.wantStaged {
					t.Errorf("staged = %v, want %q", got, tc.wantStaged)
				}
			}
		})
	}
}

// TestUpdateStatusNilStatus covers the auto-update-disabled deployment: the
// endpoint must still report the running version rather than 404 or an empty
// object, so the dashboard can show which build is live.
func TestUpdateStatusNilStatus(t *testing.T) {
	h := newUpdateHandlers(t, nil, "v0.9.0")

	code, body := getUpdateStatus(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := body["current"]; got != "v0.9.0" {
		t.Errorf("current = %v, want the build version v0.9.0 even with no Status", got)
	}
	if got := body["enabled"]; got != false {
		t.Errorf("enabled = %v, want false when no Status is wired", got)
	}
	if got := body["action"]; got != "none" {
		t.Errorf("action = %v, want none", got)
	}
}

// TestUpdateStatusErrorsAreSanitized: these strings are rendered in a browser,
// and a download failure carries a pre-signed GitHub asset URL.
func TestUpdateStatusErrorsAreSanitized(t *testing.T) {
	// The sanitizing itself is covered in selfupdate's own tests; what this
	// asserts is that the HTTP layer serves the SANITIZED field rather than
	// re-deriving a raw message of its own.
	st := selfupdate.NewStatusFixture(selfupdate.StatusFixture{
		Current:  "v1.0.0",
		CheckErr: "Get \"https://objects.githubusercontent.com/x?…\": timeout",
	})
	h := newUpdateHandlers(t, st, "v1.0.0")

	_, body := getUpdateStatus(t, h)
	msg, _ := body["check_error"].(string)
	if msg == "" {
		t.Fatal("check_error empty; the failure should be reported to the operator")
	}
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("check_error carries signed-URL credentials: %s", msg)
	}
}
