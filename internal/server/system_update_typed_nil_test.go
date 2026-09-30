package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/selfupdate"
	"github.com/naozhi/naozhi/internal/session"
)

// With the update checker disabled (a nil *selfupdate.Checker), the wiring must
// hand the system handlers a nil interface: a boxed nil would read as enabled
// and offer an install that cannot run.
func TestBuildSystemHandlers_NilCheckerReadsDisabled(t *testing.T) {
	for name, opts := range map[string]ServerOptions{
		"no status":   {},
		"with status": {Update: UpdateOptions{Status: selfupdate.NewStatus("v1.0.0")}},
	} {
		h := buildSystemHandlers(opts, session.NewRouter(session.RouterConfig{}))
		rec := httptest.NewRecorder()
		h.HandleUpdateStatus(rec, httptest.NewRequest(http.MethodGet, "/api/system/update", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", name, rec.Code, rec.Body)
		}
		var got struct {
			Enabled        bool `json:"enabled"`
			InstallEnabled bool `json:"install_enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Enabled || got.InstallEnabled {
			t.Errorf("%s: a disabled checker reads enabled=%v install_enabled=%v", name, got.Enabled, got.InstallEnabled)
		}
	}
}
