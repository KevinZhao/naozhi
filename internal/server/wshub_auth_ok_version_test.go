package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/node"
)

// Every auth path's auth_ok names the version the served page's
// nz-asset-version meta carries: the dashboard reloads when the two differ,
// so a mismatch here would reload every tab on every connect.
func TestWS_AuthOKCarriesServedAssetVersion(t *testing.T) {
	m := assetVersionMetaRe.FindStringSubmatch(servedDashboardPage(t))
	if m == nil {
		t.Fatal("served page carries no nz-asset-version meta")
	}
	cookie := http.Header{}
	cookie.Set("Cookie", auth.AuthCookieName+"="+testCookieMAC("secret"))
	for _, tc := range []struct {
		name, hubToken, sentToken string
		header                    http.Header
	}{
		{"token", "secret", "secret", nil},
		{"cookie", "secret", "", cookie},
		{"no token configured", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, _ := newTestHub(t, tc.hubToken)
			url, cleanup := startWSServer(t, hub)
			defer cleanup()
			conn, _, err := websocket.DefaultDialer.Dial(url, tc.header)
			if err != nil {
				t.Fatalf("ws dial: %v", err)
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			wsWrite(t, conn, node.ClientMsg{Type: "auth", Token: tc.sentToken})
			var frame struct {
				Type         string `json:"type"`
				AssetVersion string `json:"asset_version"`
			}
			if err := conn.ReadJSON(&frame); err != nil {
				t.Fatalf("ws read: %v", err)
			}
			if frame.Type != "auth_ok" || frame.AssetVersion != m[1] {
				t.Errorf("got %+v, want auth_ok with asset_version %s", frame, m[1])
			}
		})
	}
}
