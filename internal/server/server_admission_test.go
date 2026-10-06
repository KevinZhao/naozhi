package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// admittingPlatform records the order of Start's calls and the AdmitFunc it
// was handed; its Start fails so the test never serves.
type admittingPlatform struct {
	*mockPlatform
	calls []string
	admit platform.AdmitFunc
}

func (a *admittingPlatform) SetAdmission(fn platform.AdmitFunc) {
	a.calls = append(a.calls, "SetAdmission")
	a.admit = fn
}

func (a *admittingPlatform) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {
	a.calls = append(a.calls, "RegisterRoutes")
}

func (a *admittingPlatform) Start(platform.MessageHandler) error {
	a.calls = append(a.calls, "Start")
	return errors.New("stop here")
}

func (a *admittingPlatform) Stop() error { return nil }

// Start hands an Admitter the dispatcher's live policy check before the
// platform can deliver its first message.
func TestStart_WiresAdmissionBeforeRoutes(t *testing.T) {
	t.Parallel()
	plat := &admittingPlatform{mockPlatform: &mockPlatform{}}
	srv := NewWithOptions(ServerOptions{
		Addr:      "127.0.0.1:0",
		Router:    session.NewRouter(session.RouterConfig{}),
		Platforms: map[string]platform.Platform{"test": plat},
	})
	srv.dispatcher.SetAccessPolicy(&imauth.Policy{Rules: map[string]imauth.Rule{
		"test": {Allowed: map[string]struct{}{"alice": {}}},
	}})

	_ = srv.Start(context.Background())

	if got := plat.calls; len(got) != 3 || got[0] != "SetAdmission" || got[1] != "RegisterRoutes" {
		t.Fatalf("calls = %q, want SetAdmission before RegisterRoutes and Start", got)
	}
	msg := platform.IncomingMessage{Platform: "test", UserID: "eve", ChatID: "c", ChatType: "group", MentionMe: true}
	if plat.admit(context.Background(), msg) {
		t.Error("admission let a sender outside allowed_users through")
	}
	msg.UserID = "alice"
	if !plat.admit(context.Background(), msg) {
		t.Error("admission refused an allowed sender")
	}
}
