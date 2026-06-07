package dispatch

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestHandleCdCommand_WorkspacePersistedAfterReset is a regression guard for
// R20260607-CR-020: the old ordering was SetWorkspace then ResetChat, so the
// override written by SetWorkspace was immediately deleted by ResetChat's
// unconditional delete(wsStore.overrides[chatKey]). The correct ordering is
// ResetChat first (closes old session + deletes old override), then
// SetWorkspace (writes new override), so subsequent spawns see the new path.
func TestHandleCdCommand_WorkspacePersistedAfterReset(t *testing.T) {
	t.Parallel()

	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)

	msg := incomingMsg("/cd " + tmpDir)
	d.handleCdCommand(context.Background(), msg, "/cd "+tmpDir, slog.Default())

	if !strings.Contains(fp.lastReply(), "已切换") {
		t.Fatalf("expected workspace-changed reply, got %q", fp.lastReply())
	}

	// After /cd the router must record the new workspace override for this
	// chat so the next spawned session uses tmpDir. If ResetChat runs AFTER
	// SetWorkspace it silently deletes the override, and Workspace returns
	// "" (the router default) instead of tmpDir.
	chatKey := session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
	got := d.router.Workspace(chatKey)
	if got != tmpDir {
		t.Errorf("router.Workspace(%q) = %q, want %q — SetWorkspace override was deleted by ResetChat (R20260607-CR-020)",
			chatKey, got, tmpDir)
	}
}

// TestHandleCdCommand_WorkspaceReplacesExistingOverride verifies that a
// second /cd in the same chat replaces the previous override (not a no-op).
func TestHandleCdCommand_WorkspaceReplacesExistingOverride(t *testing.T) {
	t.Parallel()

	dir1, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks dir1: %v", err)
	}
	dir2, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks dir2: %v", err)
	}

	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)

	msg1 := incomingMsg("/cd " + dir1)
	d.handleCdCommand(context.Background(), msg1, "/cd "+dir1, slog.Default())

	msg2 := incomingMsg("/cd " + dir2)
	d.handleCdCommand(context.Background(), msg2, "/cd "+dir2, slog.Default())

	chatKey := session.ChatKey(msg1.Platform, msg1.ChatType, msg1.ChatID)
	got := d.router.Workspace(chatKey)
	if got != dir2 {
		t.Errorf("second /cd: router.Workspace(%q) = %q, want %q", chatKey, got, dir2)
	}
}
