package agentcore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Payload is the injection envelope sent to the bootstrap handler inside the
// microVM (RFC §4.1). Field set mirrors spike/agentcore/bootstrap.payload —
// the two sides must stay wire-compatible.
//
// Secrets never ride in this struct: the run-record red line (RFC §5.1)
// requires payload snapshots to be safe to persist verbatim. When secret
// injection lands (B5 enhancement tier), it will be a separate, never-
// persisted side channel resolved at send time.
type Payload struct {
	// Settings is written verbatim to ~/.claude/settings.json in the microVM.
	Settings json.RawMessage `json:"settings,omitempty"`
	// ClaudeMD is written to <workdir>/CLAUDE.md when non-empty.
	ClaudeMD string `json:"claude_md,omitempty"`
	// Prompt is the single user turn (run-once job model, RFC §3.2).
	Prompt string `json:"prompt"`
	// Model overrides the CLI --model flag when non-empty.
	Model string `json:"model,omitempty"`
	// Env carries extra environment for the CLI process (e.g. AWS_REGION).
	// Keys must be non-empty and '='-free; the bootstrap rejects others.
	Env map[string]string `json:"env,omitempty"`
}

// maxPayloadBytes is the InvokeAgentRuntime payload ceiling (100MB, 2026-06
// verified). Marshal fails fast on oversize instead of letting the API
// reject with an opaque 4xx.
const maxPayloadBytes = 100 << 20

// Marshal encodes the payload and enforces the platform size ceiling.
func (p *Payload) Marshal() ([]byte, error) {
	if p.Prompt == "" {
		return nil, fmt.Errorf("agentcore: payload prompt is required")
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

// NewRunID returns a fresh runtimeSessionId for a run-once job.
//
// Two hard constraints meet here:
//   - RFC §4.1: the id must be unique per job — reuse would stick the job
//     to a previous, un-burned microVM and break isolation;
//   - validation F3: the InvokeAgentRuntime API rejects ids shorter than
//     33 characters.
//
// Format: "run-<unixnano>-<8 random hex bytes>" — ~40 chars, sortable by
// launch time, no coordination needed. crypto/rand.Read is documented to
// never fail (Go ≥1.24), so there is no degraded path that could collide.
func NewRunID(now time.Time) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("run-%d-%s", now.UnixNano(), hex.EncodeToString(b[:]))
}
