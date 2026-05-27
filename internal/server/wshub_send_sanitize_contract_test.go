package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRemoteWSError_LogsGoThroughSanitizeForLog pins R217-CR-5 (#641):
// handleRemoteInterrupt and handleRemoteSend forward to a peer node via
// nc.ProxyInterruptSession / nc.Send. The error those return originates
// from a remote / transport stack — a compromised peer node can return an
// err string padded with C1 controls / bidi overrides / LS+PS that
// byte-level `<0x20` gates miss. The reverse direction
// (internal/upstream/connector_rpc.go LogSystemEvent site) already routes
// the error through osutil.SanitizeForLog before it reaches slog +
// dashboard rendering; the forward direction must do the same so a
// hostile peer cannot inject log lines into the primary's slog sink.
//
// Source-level scan instead of an HTTP / Hub round-trip because the path
// requires a fully wired Hub + multi-node setup; a contract test catches
// any future revert that reverts to `"err", err` without depending on a
// fragile integration harness.
func TestRemoteWSError_LogsGoThroughSanitizeForLog(t *testing.T) {
	t.Parallel()
	body := readWshubSendSource(t)

	// Negative: the legacy unsanitized form ("err", err) at either of the
	// two slog.Error sites would re-open the asymmetry. Anchor on the
	// surrounding "remote ws ... failed" message so a generic ("err", err)
	// elsewhere in the file (e.g. a panic-recovery branch) doesn't fool
	// the gate.
	for _, anchor := range []string{
		`"remote ws interrupt failed"`,
		`"remote ws send failed"`,
	} {
		idx := strings.Index(body, anchor)
		if idx < 0 {
			t.Fatalf("wshub_send.go no longer contains %s — update this contract test.", anchor)
		}
		// Slice 256 bytes after the anchor: covers the slog.Error call
		// without bleeding into the next handler.
		end := idx + 256
		if end > len(body) {
			end = len(body)
		}
		window := body[idx:end]
		if !strings.Contains(window, "osutil.SanitizeForLog(err.Error()") {
			t.Errorf("R217-CR-5 (#641): the slog.Error at %s must route err through\n"+
				"osutil.SanitizeForLog (mirror upstream/connector_rpc.go). Window:\n%s",
				anchor, window)
		}
	}
}

func readWshubSendSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(thisFile), "wshub_send.go")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read wshub_send.go: %v", err)
	}
	return string(data)
}
