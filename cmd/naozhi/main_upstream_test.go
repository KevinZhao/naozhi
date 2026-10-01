package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
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
	var entries []clievent.EventEntry
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
	router := session.NewRouter(session.RouterConfig{})
	t.Cleanup(router.Shutdown)
	discover := newUpstreamDiscoverFunc(claudeDir, router, nil)

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

// Without a Claude projects dir neither RPC is wired; with one, both are.
func TestUpstreamDiscovery(t *testing.T) {
	if d := upstreamDiscovery("", nil, nil); d.Sessions != nil || d.Preview != nil {
		t.Error("no claude dir: discovery RPCs wired anyway")
	}
	if d := upstreamDiscovery(t.TempDir(), nil, nil); d.Sessions == nil || d.Preview == nil {
		t.Error("claude dir set: a discovery RPC is missing")
	}
}

// TestNewUpstreamPreviewFunc_SendsTheWireView: the preview payload goes to the
// primary's dashboard, so a credential in the transcript is redacted.
func TestNewUpstreamPreviewFunc_SendsTheWireView(t *testing.T) {
	t.Parallel()
	const (
		sessionID = "00000000-0000-0000-0000-0000000013b1"
		cwd       = "/home/u/wire-view"
		secret    = "sk-ant-api03-PPPPPPPPPPPPPPPPPPPPPPPP"
	)
	claudeDir := t.TempDir()
	path := claudefs.SessionJSONL(claudeDir, cwd, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"my key is ` + secret + `"}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	raw, err := newUpstreamPreviewFunc(claudeDir)(sessionID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !strings.Contains(string(raw), "my key is") {
		t.Fatalf("preview lost the entry: %s", raw)
	}
	for _, leak := range []string{"jsonl_path", secret} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("preview payload carries %q: %s", leak, raw)
		}
	}
}
