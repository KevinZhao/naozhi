package agentcore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Payload is the injection envelope sent to the bootstrap handler inside the
// microVM; mirrors spike/agentcore/bootstrap.payload (wire-compatible).
// Secrets never ride here: snapshots must be safe to persist verbatim.
type Payload struct {
	// Settings is written verbatim to ~/.claude/settings.json in the microVM.
	Settings json.RawMessage `json:"settings,omitempty"`
	// ClaudeMD is written to <workdir>/CLAUDE.md when non-empty.
	ClaudeMD string `json:"claude_md,omitempty"`
	// Prompt is the single user turn (run-once job model).
	Prompt string `json:"prompt"`
	// Model overrides the CLI --model flag when non-empty.
	Model string `json:"model,omitempty"`
	// Env carries extra environment for the CLI; keys non-empty and '='-free.
	Env map[string]string `json:"env,omitempty"`
}

// maxPayloadBytes is the InvokeAgentRuntime payload ceiling (100MB).
const maxPayloadBytes = 100 << 20

// Marshal encodes the payload and enforces the platform size ceiling.
func (p *Payload) Marshal() ([]byte, error) {
	if p.Prompt == "" {
		return nil, fmt.Errorf("agentcore: payload prompt is required")
	}
	// '=' breaks KEY=VALUE encoding; NUL is illegal in C env strings.
	for k := range p.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return nil, fmt.Errorf("agentcore: invalid env key %q: must be non-empty and free of '=' and NUL", k)
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("agentcore: marshal payload: %w", err)
	}
	if len(b) > maxPayloadBytes {
		return nil, fmt.Errorf("agentcore: payload %d bytes exceeds %d limit", len(b), maxPayloadBytes)
	}
	return b, nil
}

// NewRunID returns a fresh runtimeSessionId: unique per job (reuse would
// stick the job to a previous, un-burned microVM) and ≥33 chars (API
// minimum). "run-<unixnano>-<8 random hex bytes>" is ~40 chars and sortable.
func NewRunID(now time.Time) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("run-%d-%s", now.UnixNano(), hex.EncodeToString(b[:]))
}
