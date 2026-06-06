package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// TestNewUpstreamPreviewFunc_EmptyOnError verifies the preview callback's
// non-nil-array contract (R237-ARCH-8 / #590): a missing/unreadable session
// transcript must marshal to "[]" rather than "null" or surfacing an error,
// so the connector never forwards a null JSON payload downstream.
func TestNewUpstreamPreviewFunc_EmptyOnError(t *testing.T) {
	t.Parallel()

	claudeDir := t.TempDir() // empty: no transcripts exist
	preview := newUpstreamPreviewFunc(claudeDir)

	raw, err := preview("does-not-exist-session-id")
	if err != nil {
		t.Fatalf("preview returned err = %v, want nil (errors are swallowed to []) ", err)
	}
	var entries []cli.EventEntry
	if uerr := json.Unmarshal(raw, &entries); uerr != nil {
		t.Fatalf("preview payload is not a JSON array: %q (%v)", raw, uerr)
	}
	if string(raw) == "null" {
		t.Fatalf("preview payload must be [] not null")
	}
	if len(entries) != 0 {
		t.Errorf("preview for missing session = %d entries, want 0", len(entries))
	}
}

// TestNewUpstreamDiscoverFunc_EmptyArrayOnScanError verifies the discover
// callback's non-nil-array contract: scanning a non-existent claude dir
// yields a valid empty JSON array, not null or an error. A zero-value
// Router (no managed sessions) supplies empty exclude sets, and a nil
// projectMgr exercises the "skip project backfill" branch. R237-ARCH-8.
func TestNewUpstreamDiscoverFunc_EmptyArrayOnScanError(t *testing.T) {
	t.Parallel()

	// Point at a path that does not exist so discovery.Scan errors out and
	// the func falls back to marshalling an empty array.
	claudeDir := filepath.Join(t.TempDir(), "no-such-claude-dir")
	discover := newUpstreamDiscoverFunc(claudeDir, &session.Router{}, nil)

	raw, err := discover()
	if err != nil {
		t.Fatalf("discover returned err = %v, want nil (errors swallowed to [])", err)
	}
	if string(raw) == "null" {
		t.Fatalf("discover payload must be [] not null, got %q", raw)
	}
	var arr []json.RawMessage
	if uerr := json.Unmarshal(raw, &arr); uerr != nil {
		t.Fatalf("discover payload is not a JSON array: %q (%v)", raw, uerr)
	}
}
