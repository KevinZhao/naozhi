package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

// TestSetSlogKey_AttachesSessionAttr ensures Process.SetSlogKey binds a
// session attr that subsequent slogger() calls carry into log output.
// Guards R70-ARCH-M3 so future refactors of readLoop/heartbeatLoop logging
// can't silently drop the session=key tag that oncall uses to attribute
// shim disconnect / readloop panic entries across many concurrent sessions.
func TestSetSlogKey_AttachesSessionAttr(t *testing.T) {
	var buf bytes.Buffer
	// Swap the default logger for a capture-only handler. Restore on exit so
	// other tests in this binary don't inherit the recorder.
	old := slog.Default()
	defer slog.SetDefault(old)
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Construct a bare Process. We don't drive a shim here — we only need
	// slogger() to reflect the SetSlogKey call.
	p := &Process{}
	// Before SetSlogKey the fallback is slog.Default().
	p.slogger().Info("pre")
	if !strings.Contains(buf.String(), "msg=pre") || strings.Contains(buf.String(), "session=") {
		t.Fatalf("pre-Set log should not contain session attr, got %q", buf.String())
	}

	buf.Reset()
	p.SetSlogKey("feishu:group:oc_abc:general")
	p.slogger().Info("post")
	if !strings.Contains(buf.String(), `session=feishu:group:oc_abc:general`) {
		t.Errorf("post-Set log missing session attr, got %q", buf.String())
	}
}

// TestSetSlogKey_EmptyIsNoop prevents accidental nil-deref paths where
// callers pass "" and expect slogger() to still work.
func TestSetSlogKey_EmptyIsNoop(t *testing.T) {
	p := &Process{}
	p.SetSlogKey("")
	if l := p.log.Load(); l != nil {
		t.Errorf("empty key should not populate p.log, got %v", l)
	}
	// slogger() must never return nil — it falls back to slog.Default.
	if p.slogger() == nil {
		t.Error("slogger() returned nil after empty SetSlogKey")
	}
}

// Sanity check: atomic.Pointer[slog.Logger] zero value is usable and Load
// returns nil (not a panic). This is a freeze test so if we ever change
// p.log's type we catch the initialisation-order assumption immediately.
func TestProcessLogPointerZeroValue(t *testing.T) {
	var p Process
	var ptr atomic.Pointer[slog.Logger]
	if p.log != ptr {
		// Can't compare atomic.Pointer directly; use Load and expect same nil.
	}
	if p.log.Load() != nil {
		t.Error("zero-value atomic.Pointer[Logger] should Load nil")
	}
}
