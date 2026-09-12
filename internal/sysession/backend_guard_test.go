package sysession

import (
	"strings"
	"testing"
)

// TestNewRunner_RefusesNonClaudeBackend: runnerImplBaseArgs is Claude's one-shot
// argv. Before this guard, a deployment with cli.default = kiro handed kiro's
// binary `-p --output-format json --setting-sources ""` — which kiro rejects,
// since it speaks ACP — so every daemon tick failed with a message about argv
// while the cost was still booked to "claude". The failure is now at
// construction, naming the backend choice.
func TestNewRunner_RefusesNonClaudeBackend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := NewRunner(RunnerConfig{BinPath: "claude", BackendID: "kiro", WorkDir: dir})
	if err == nil {
		t.Fatal("want an error for a non-claude backend")
	}
	for _, want := range []string{"kiro", "claude"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}

// TestNewRunner_EmptyBackendIDMeansClaude keeps callers that predate the field
// working, and pins that "unset" is not read as "some other backend".
func TestNewRunner_EmptyBackendIDMeansClaude(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, err := NewRunner(RunnerConfig{BinPath: "claude", BackendID: "", WorkDir: dir})
	if err != nil {
		t.Fatalf("empty BackendID should be accepted: %v", err)
	}
	impl, ok := r.(*runnerImpl)
	if !ok {
		t.Fatalf("NewRunner returned %T", r)
	}
	if got := impl.backendID(); got != BackendClaude {
		t.Errorf("backendID() = %q, want %q", got, BackendClaude)
	}
}

// TestNewRunner_AcceptsClaudeExplicitly: the guard must not reject the backend it
// exists to protect.
func TestNewRunner_AcceptsClaudeExplicitly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := NewRunner(RunnerConfig{BinPath: "claude", BackendID: BackendClaude, WorkDir: dir}); err != nil {
		t.Fatalf("claude should be accepted: %v", err)
	}
}

// TestNewVisionRunner_InheritsTheBackendGuard: image auto-orient is on by default
// (ImageOrientConfig.Enabled is *bool, nil = true), so this path breaks for every
// kiro-default deployment, not only ones that opted into daemons.
func TestNewVisionRunner_InheritsTheBackendGuard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := NewVisionRunner(RunnerConfig{BinPath: "claude", BackendID: "kiro", WorkDir: dir}); err == nil {
		t.Fatal("NewVisionRunner must inherit NewRunner's backend guard")
	}
}

// TestBookRunCostAttributesTheRealBackend: the ledger entry used to carry the
// literal "claude" regardless of which binary ran.
func TestBookRunCostAttributesTheRealBackend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, err := NewRunner(RunnerConfig{BinPath: "claude", BackendID: BackendClaude, WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	impl := r.(*runnerImpl)
	if got := impl.backendID(); got != BackendClaude {
		t.Errorf("backendID() = %q, want %q", got, BackendClaude)
	}
	// The guard means a running Runner is always claude today, so this asserts the
	// attribution reads the field rather than a literal: a future backend that can
	// run the one-shot argv books under its own id without touching runner_cost.go.
	impl.cfg.BackendID = "future-backend"
	if got := impl.backendID(); got != "future-backend" {
		t.Errorf("backendID() = %q; attribution must read cfg, not a literal", got)
	}
}
