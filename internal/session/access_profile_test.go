package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveEnvOverlay_LiteralAndFileExpansion(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "anthropic.token")
	if err := os.WriteFile(tokenPath, []byte("sk-secret-123\n\n"), 0600); err != nil {
		t.Fatal(err)
	}

	overlay, err := resolveEnvOverlay(map[string]string{
		"CLAUDE_CODE_USE_BEDROCK":   "0",
		"ANTHROPIC_BASE_URL":        "https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN_FILE": tokenPath,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overlay["CLAUDE_CODE_USE_BEDROCK"] != "0" {
		t.Errorf("literal not copied: %q", overlay["CLAUDE_CODE_USE_BEDROCK"])
	}
	if overlay["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Errorf("literal url not copied: %q", overlay["ANTHROPIC_BASE_URL"])
	}
	// *_FILE expands to the concrete key with trailing newlines trimmed.
	if overlay["ANTHROPIC_AUTH_TOKEN"] != "sk-secret-123" {
		t.Errorf("token not expanded/trimmed: %q", overlay["ANTHROPIC_AUTH_TOKEN"])
	}
	// The *_FILE key itself must NOT be forwarded to the subprocess.
	if _, ok := overlay["ANTHROPIC_AUTH_TOKEN_FILE"]; ok {
		t.Errorf("*_FILE key leaked into overlay")
	}
}

func TestResolveEnvOverlay_MissingFileFailsLoud(t *testing.T) {
	_, err := resolveEnvOverlay(map[string]string{
		"ANTHROPIC_AUTH_TOKEN_FILE": "/nonexistent/path/to/token",
	})
	if err == nil {
		t.Fatal("expected fail-loud error for missing *_FILE, got nil")
	}
}

func TestResolveEnvOverlay_EmptyReturnsNil(t *testing.T) {
	got, err := resolveEnvOverlay(nil)
	if err != nil || got != nil {
		t.Errorf("empty overlay: got (%v, %v), want (nil, nil)", got, err)
	}
}

func TestAccessProfileInfos(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.token")
	if err := os.WriteFile(present, []byte("sk-x"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &Router{
		accessProfiles: map[string]AccessProfile{
			"bedrock": {
				DisplayName:  "Bedrock · Opus",
				ChipColor:    "#7c5cff",
				DefaultModel: "claude-opus-4-8",
				Env:          map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"},
			},
			"1p-ok": {
				DisplayName: "1P · Fable",
				Env:         map[string]string{"ANTHROPIC_AUTH_TOKEN_FILE": present},
			},
			"1p-broken": {
				DisplayName: "1P · Broken",
				Env:         map[string]string{"ANTHROPIC_AUTH_TOKEN_FILE": filepath.Join(dir, "missing")},
			},
		},
	}
	infos := r.AccessProfileInfos()
	if len(infos) != 3 {
		t.Fatalf("want 3 infos, got %d", len(infos))
	}
	// Sorted by ID: 1p-broken, 1p-ok, bedrock.
	byID := map[string]AccessProfileInfo{}
	for _, in := range infos {
		byID[in.ID] = in
	}
	if !byID["bedrock"].SecretOK {
		t.Error("bedrock has no *_FILE, SecretOK should be true")
	}
	if !byID["1p-ok"].SecretOK {
		t.Error("1p-ok token exists, SecretOK should be true")
	}
	if byID["1p-broken"].SecretOK {
		t.Error("1p-broken token missing, SecretOK should be false")
	}
	if byID["bedrock"].DisplayName != "Bedrock · Opus" || byID["bedrock"].ChipColor != "#7c5cff" {
		t.Errorf("display fields wrong: %+v", byID["bedrock"])
	}
	// SECURITY: the info projection must NOT expose env values. Serialise and
	// assert no overlay value leaked (the bedrock selector "1", token contents).
	blob := fmt.Sprintf("%+v", infos)
	if strings.Contains(blob, "CLAUDE_CODE_USE_BEDROCK") || strings.Contains(blob, "sk-x") ||
		strings.Contains(blob, "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("AccessProfileInfo leaked env content: %s", blob)
	}
}

func TestAccessProfileInfos_EmptyRegistry(t *testing.T) {
	r := &Router{}
	if got := r.AccessProfileInfos(); got != nil {
		t.Errorf("empty registry should return nil, got %v", got)
	}
}

func TestAddAccessProfile(t *testing.T) {
	r := &Router{accessProfiles: map[string]AccessProfile{
		"existing": {DisplayName: "Existing"},
	}}
	if !r.HasAccessProfile("existing") {
		t.Fatal("existing profile should be present")
	}
	if r.HasAccessProfile("new") {
		t.Fatal("new profile should be absent before add")
	}
	if err := r.AddAccessProfile("new", AccessProfile{DisplayName: "New", DefaultModel: "m"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !r.HasAccessProfile("new") {
		t.Error("new profile should be present after add")
	}
	// Existing untouched (copy-on-write preserves prior entries).
	if !r.HasAccessProfile("existing") {
		t.Error("existing profile lost after add")
	}
	// Duplicate rejected.
	if err := r.AddAccessProfile("new", AccessProfile{}); err == nil {
		t.Error("duplicate add should be rejected")
	}
	// Empty id rejected.
	if err := r.AddAccessProfile("", AccessProfile{}); err == nil {
		t.Error("empty id should be rejected")
	}
}

func TestAddAccessProfile_NilMapBootstrap(t *testing.T) {
	r := &Router{} // nil accessProfiles
	if err := r.AddAccessProfile("first", AccessProfile{DisplayName: "First"}); err != nil {
		t.Fatalf("add to nil map: %v", err)
	}
	if !r.HasAccessProfile("first") {
		t.Error("profile not registered into freshly-bootstrapped map")
	}
}
