package server

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// TestWS_SendUnknownFileIDHintsReattach pins F5: the WS path still accepts
// file_ids (third-party / legacy clients) with the upgrade-time frozen owner,
// so a TakeAll miss must tell the user to re-attach + resend rather than just
// "file not found or expired".
func TestWS_SendUnknownFileIDHintsReattach(t *testing.T) {
	hub, _ := newTestHub("")
	hub.SetUploadStore(newUploadStore())
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "send", Key: "test:d:u:general", Text: "hi", ID: "r1", FileIDs: []string{"nope"}})
	resp := readUntilType(t, conn, "send_ack")
	if resp.Status != "error" {
		t.Fatalf("status = %q, want error", resp.Status)
	}
	if !strings.Contains(resp.Error, "file not found or expired") || !strings.Contains(resp.Error, "重新添加") {
		t.Fatalf("error = %q, want the not-found label plus a re-attach hint", resp.Error)
	}
}
