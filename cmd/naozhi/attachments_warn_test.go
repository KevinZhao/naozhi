package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sysession"
)

type staticRoots []string

func (r staticRoots) KnownWorkspaceRoots() []string { return r }

// writeAttachment puts a size-byte payload under root's attachment tree.
func writeAttachment(t *testing.T, root, name string, size int) {
	t.Helper()
	dir := filepath.Join(root, attachment.Dir, "2026-01-01")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

// warnRecords returns the attributes of every captured record with message msg.
func warnRecords(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// TestWarnIfAttachmentsOver_SumsEveryRoot: neither root alone reaches the
// threshold, their sum does, and a root without an attachment tree is skipped.
func TestWarnIfAttachmentsOver_SumsEveryRoot(t *testing.T) {
	small, big, bare := t.TempDir(), t.TempDir(), t.TempDir()
	writeAttachment(t, small, "a.pdf", 3000)
	writeAttachment(t, big, "b.png", 4000)
	writeAttachment(t, big, "c.png", 1000)
	roots := staticRoots{small, bare, big}

	buf := captureSlog(t)
	warnIfAttachmentsOver(roots, attachmentGCDisabled, 8001, osutil.StateDirSize)
	if got := warnRecords(t, buf, "attachments large"); len(got) != 0 {
		t.Fatalf("8000 bytes under an 8001-byte threshold warned: %v", got)
	}

	buf = captureSlog(t)
	warnIfAttachmentsOver(roots, attachmentGCDisabled, 8000, osutil.StateDirSize)
	recs := warnRecords(t, buf, "attachments large")
	if len(recs) != 1 {
		t.Fatalf("want one warn at the 8000-byte sum, got %d: %s", len(recs), buf)
	}
	rec := recs[0]
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	if rec["roots"] != float64(2) {
		t.Errorf("roots = %v, want 2 (the bare root has no attachment tree)", rec["roots"])
	}
	if rec["largest_root"] != big {
		t.Errorf("largest_root = %v, want %s", rec["largest_root"], big)
	}
	if rec["truncated"] != false {
		t.Errorf("truncated = %v, want false", rec["truncated"])
	}
}

func TestWarnIfAttachmentsOver_SilentWithoutTrees(t *testing.T) {
	buf := captureSlog(t)
	warnIfAttachmentsOver(staticRoots{t.TempDir(), filepath.Join(t.TempDir(), "gone")}, attachmentGCDisabled, 1, osutil.StateDirSize)
	if got := warnRecords(t, buf, "attachments large"); len(got) != 0 {
		t.Fatalf("roots without attachment trees warned: %v", got)
	}
}

// TestWarnIfAttachmentsOver_HintFollowsGCMode: the hint names the step the
// operator has not taken yet for each daemon mode.
func TestWarnIfAttachmentsOver_HintFollowsGCMode(t *testing.T) {
	root := t.TempDir()
	writeAttachment(t, root, "a.pdf", 10)
	for mode, want := range map[string]string{
		attachmentGCDisabled: "set sysession.enabled and sysession.daemons.attachment-gc.enabled",
		attachmentGCDryRun:   "set dry_run: false",
		attachmentGCEnabled:  "per_root_cap may be too loose",
	} {
		buf := captureSlog(t)
		warnIfAttachmentsOver(staticRoots{root}, mode, 10, osutil.StateDirSize)
		recs := warnRecords(t, buf, "attachments large")
		if len(recs) != 1 {
			t.Fatalf("mode %s: want one warn, got %d", mode, len(recs))
		}
		if recs[0]["attachment_gc"] != mode {
			t.Errorf("mode %s: attachment_gc = %v", mode, recs[0]["attachment_gc"])
		}
		hint, _ := recs[0]["hint"].(string)
		if !strings.Contains(hint, want) {
			t.Errorf("mode %s: hint %q lacks %q", mode, hint, want)
		}
	}
}

func TestAttachmentGCMode(t *testing.T) {
	gc := func(enabled, dryRun bool) map[string]config.SysessionDaemonConfig {
		return map[string]config.SysessionDaemonConfig{
			sysession.DaemonAttachmentGC: {Enabled: enabled, DryRun: dryRun},
		}
	}
	tests := []struct {
		name      string
		sysession bool
		daemons   map[string]config.SysessionDaemonConfig
		want      string
	}{
		{"sysession_off", false, gc(true, false), attachmentGCDisabled},
		{"daemon_missing", true, map[string]config.SysessionDaemonConfig{sysession.DaemonAutoTitler: {Enabled: true}}, attachmentGCDisabled},
		{"daemon_not_enabled", true, gc(false, true), attachmentGCDisabled},
		{"enabled_dry_run", true, gc(true, true), attachmentGCDryRun},
		{"enabled_live", true, gc(true, false), attachmentGCEnabled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Sysession.Enabled = tc.sysession
			cfg.Sysession.Daemons = tc.daemons
			if got := attachmentGCMode(cfg); got != tc.want {
				t.Fatalf("attachmentGCMode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWarnIfStateDirOver_HintMatchesStateDir: the state-dir hint names its own
// events and, because the default session.cwd sits under ~/.naozhi, the
// attachments warning too.
func TestWarnIfStateDirOver_HintMatchesStateDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), make([]byte, 10), 0o600); err != nil {
		t.Fatal(err)
	}
	buf := captureSlog(t)
	warnIfStateDirOver(dir, 10)
	recs := warnRecords(t, buf, "state directory large")
	if len(recs) != 1 {
		t.Fatalf("want one warn, got %d: %s", len(recs), buf)
	}
	hint, _ := recs[0]["hint"].(string)
	for _, want := range []string{"events/*.log", "session.cwd", "attachments large"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q lacks %q", hint, want)
		}
	}
}

// TestWarnIfAttachmentsOver_TruncatedWalkCounts: a walk cut off at its entry
// budget still adds its partial total and marks the warn truncated, while a
// missing tree adds nothing.
func TestWarnIfAttachmentsOver_TruncatedWalkCounts(t *testing.T) {
	cut, whole, gone := t.TempDir(), t.TempDir(), t.TempDir()
	sizes := map[string]struct {
		n   int64
		err error
	}{
		filepath.Join(cut, attachment.Dir):   {300 << 20, osutil.ErrStateDirScanTruncated},
		filepath.Join(whole, attachment.Dir): {200 << 20, nil},
		filepath.Join(gone, attachment.Dir):  {0, fs.ErrNotExist},
	}
	treeSize := func(p string) (int64, error) { return sizes[p].n, sizes[p].err }

	buf := captureSlog(t)
	warnIfAttachmentsOver(staticRoots{cut, whole, gone}, attachmentGCDisabled, 500<<20, treeSize)
	recs := warnRecords(t, buf, "attachments large")
	if len(recs) != 1 {
		t.Fatalf("want one warn from the 300+200 MiB sum, got %d: %s", len(recs), buf)
	}
	rec := recs[0]
	if rec["total_mb"] != float64(500) || rec["truncated"] != true || rec["roots"] != float64(2) {
		t.Errorf("total_mb=%v truncated=%v roots=%v, want 500 true 2", rec["total_mb"], rec["truncated"], rec["roots"])
	}
	if rec["largest_root"] != cut {
		t.Errorf("largest_root = %v, want the truncated root %s", rec["largest_root"], cut)
	}
}

// sparseFile sets path's apparent size to size without allocating its blocks.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

// TestStartupWarnThresholds_500MiB pins both production wrappers to 500 MiB:
// one byte short stays silent, exactly 500 MiB warns.
func TestStartupWarnThresholds_500MiB(t *testing.T) {
	const limit = 500 << 20
	stateDir := t.TempDir()
	stateFile := filepath.Join(stateDir, "sessions.json")
	root := t.TempDir()
	attFile := filepath.Join(root, attachment.Dir, "2026-01-01", "big.bin")

	for _, tc := range []struct {
		size  int64
		warns int
	}{{limit - 1, 0}, {limit, 1}} {
		sparseFile(t, stateFile, tc.size)
		sparseFile(t, attFile, tc.size)
		buf := captureSlog(t)
		warnIfStateDirLarge(stateDir)
		warnIfAttachmentsLarge(staticRoots{root}, attachmentGCDisabled)
		if got := len(warnRecords(t, buf, "state directory large")); got != tc.warns {
			t.Errorf("state dir at %d bytes: %d warns, want %d", tc.size, got, tc.warns)
		}
		if got := len(warnRecords(t, buf, "attachments large")); got != tc.warns {
			t.Errorf("attachments at %d bytes: %d warns, want %d", tc.size, got, tc.warns)
		}
	}
}
