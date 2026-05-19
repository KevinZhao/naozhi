package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
)

// resumeIDRe accepts only characters that can legally appear in a Claude
// session UUID (hex + hyphen). This is a defence-in-depth check at the CLI
// argv boundary — without it, a crafted resume_id beginning with `-` could
// be re-interpreted by the Claude CLI as a flag.
var resumeIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ClaudeProtocol implements Protocol for Claude CLI's stream-json format.
type ClaudeProtocol struct {
	// SettingsFile is passed to --settings <file>. When non-empty, standard setting
	// sources are disabled (--setting-sources "") and this file is loaded instead.
	// Use writeClaudeSettingsOverride() to generate a filtered copy of user settings
	// that strips hooks calling back into naozhi.
	SettingsFile string

	// RefreshSettings, when non-nil, is invoked at the start of every BuildArgs
	// call and its return value (if non-empty) replaces SettingsFile for that
	// spawn. This lets the override file track edits to ~/.claude/settings.json
	// at session-spawn granularity, instead of being frozen at naozhi startup.
	// Returning "" indicates "refresh failed; keep the existing SettingsFile" —
	// callers that hit a read race or IO error should not nuke the prior path
	// because the last known-good override still authenticates Bedrock.
	//
	// Clone propagates this field so per-spawn copies retain refresh ability.
	RefreshSettings func() string
}

func (p *ClaudeProtocol) Name() string { return "stream-json" }

func (p *ClaudeProtocol) Clone() Protocol {
	return &ClaudeProtocol{
		SettingsFile:    p.SettingsFile,
		RefreshSettings: p.RefreshSettings,
	}
}

func (p *ClaudeProtocol) BuildArgs(opts SpawnOptions) []string {
	if p.RefreshSettings != nil {
		if path := p.RefreshSettings(); path != "" {
			p.SettingsFile = path
		}
	}
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		// Passthrough matching depends on CLI echoing every stdin user message
		// back as an isReplay:true event with round-tripped uuid. See
		// docs/rfc/passthrough-mode.md §5.3 and validation report V3/V6.
		// Safe to always enable: replay events are filtered out of EventLog
		// (see filterReplayEvent).
		"--replay-user-messages",
		"--setting-sources", "", // disable standard settings to avoid hook loops
		"--dangerously-skip-permissions",
	}
	if p.SettingsFile != "" {
		args = append(args, "--settings", p.SettingsFile)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ResumeID != "" && resumeIDRe.MatchString(opts.ResumeID) {
		// Silently drop malformed IDs rather than erroring: the caller may
		// have passed a user-facing label; we still want a fresh session.
		args = append(args, "--resume", opts.ResumeID)
	}
	args = append(args, capExtraArgsBytes(opts.ExtraArgs)...)
	return args
}

// maxExtraArgsBytes caps the total byte length of opts.ExtraArgs joined. The
// kernel's ARG_MAX is ~2 MiB on Linux; once argv+envp+padding crosses that,
// exec returns E2BIG and the spawn fails opaquely. Realistic ExtraArgs payloads
// (e.g. scratch session --append-system-prompt with 24 KiB quote +
// project-level system prompts) stay well under 128 KiB. Drop the entire slice
// rather than truncating mid-arg, since flag-value pairs cannot be safely cut.
const maxExtraArgsBytes = 128 * 1024

// capExtraArgsBytes guards against a runaway caller (or accumulated stacked
// scratch contexts) producing an argv that exceeds ARG_MAX. Returns the input
// unchanged when within the cap; logs and returns nil when over.
func capExtraArgsBytes(extra []string) []string {
	total := 0
	for _, a := range extra {
		total += len(a) + 1 // +1 for argv NUL separator
		if total > maxExtraArgsBytes {
			slog.Warn("cli: ExtraArgs exceeds byte cap, dropping",
				"total_bytes", total, "cap", maxExtraArgsBytes, "count", len(extra))
			return nil
		}
	}
	return extra
}

func (p *ClaudeProtocol) Init(_ *JSONRW, _, _ string) (string, error) {
	return "", nil
}

func (p *ClaudeProtocol) WriteMessage(w io.Writer, text string, images []ImageData) error {
	return p.WriteUserMessageLocked(w, "", text, images, "")
}

// WriteUserMessageLocked writes a user message with optional uuid + priority.
// Caller must already hold Process.shimWMu (see protocol.go interface doc).
//
// Empty uuid / priority are omitted from the JSON (omitempty), so the payload
// is byte-identical to the legacy WriteMessage path when both are empty —
// safe for tests and ACP-backed stream-json paths that never set them.
func (p *ClaudeProtocol) WriteUserMessageLocked(w io.Writer, uuid, text string, images []ImageData, priority string) error {
	msg := NewUserMessageWithMeta(text, images, uuid, priority)
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func (p *ClaudeProtocol) SupportsPriority() bool { return true }
func (p *ClaudeProtocol) SupportsReplay() bool   { return true }

// Capabilities returns the hard-coded Caps for Claude stream-json.
// See RNEW-ARCH-404: opt-in accessor for consumers migrating off
// individual SupportsX() methods.
func (p *ClaudeProtocol) Capabilities() Caps {
	return Caps{Replay: true, Priority: true, SoftInterrupt: false, StreamJSON: true}
}

// controlRequestInterrupt is the NDJSON payload for an in-band "abort this turn"
// signal sent via stdin. The Claude CLI reacts by killing any in-flight tool
// call (bash children are SIGKILL'd), closing the current turn with a
// `stop_reason=tool_use` or `end_turn` result event, and returning to the
// ready state — without tearing down the session. Verified against CLI 2.1.119.
type controlRequestInterrupt struct {
	Type      string                      `json:"type"`
	RequestID string                      `json:"request_id"`
	Request   controlRequestInterruptBody `json:"request"`
}

type controlRequestInterruptBody struct {
	Subtype string `json:"subtype"`
}

func (p *ClaudeProtocol) WriteInterrupt(w io.Writer, requestID string) error {
	msg := controlRequestInterrupt{
		Type:      "control_request",
		RequestID: requestID,
		Request:   controlRequestInterruptBody{Subtype: "interrupt"},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal control_request: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write control_request: %w", err)
	}
	return nil
}

func (p *ClaudeProtocol) ReadEvent(line string) ([]Event, bool, error) {
	var ev Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return nil, false, err
	}
	// Skip hook events
	if ev.Type == "system" && (ev.SubType == "hook_started" || ev.SubType == "hook_response") {
		return nil, false, nil
	}
	// Skip control_response — it's a protocol-level ack for our own
	// control_request (interrupt) and carries no user-visible payload.
	// Forwarding it would confuse logEvent / EventEntriesFromEvent.
	if ev.Type == "control_response" {
		return nil, false, nil
	}
	// AskUserQuestion surfacing: in `claude -p` (headless) mode the CLI
	// auto-injects an is_error:true tool_result ~3ms after the tool_use,
	// bailing the model back to a text response inside the same turn
	// (verified in test/e2e/askuser/). We can't intercept that — but we
	// can observe the tool_use and let dispatch render an interactive
	// card so the next user turn carries the chosen option(s). The
	// AskQuestion field rides on the same assistant event so the existing
	// tool_use EventLog entry still flows through unchanged.
	if ev.Type == "assistant" && ev.Message != nil {
		if aq := extractAskQuestion(ev.Message.Content); aq != nil {
			ev.AskQuestion = aq
		}
	}
	return []Event{ev}, ev.Type == "result", nil
}

// askUserQuestionInput matches the `input` field of an AskUserQuestion tool_use
// block. Field tags match the exact keys observed in test/e2e/askuser logs.
type askUserQuestionInput struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// extractAskQuestion returns the AskQuestion payload when the content blocks
// contain a tool_use with name "AskUserQuestion" and valid input.
// Returns nil when no AQ tool_use present or the input fails to decode —
// callers treat nil as "no card to render".
func extractAskQuestion(blocks []ContentBlock) *AskQuestion {
	for _, b := range blocks {
		if b.Type != "tool_use" || b.Name != "AskUserQuestion" || len(b.Input) == 0 {
			continue
		}
		var inp askUserQuestionInput
		if err := json.Unmarshal(b.Input, &inp); err != nil {
			// Log at Debug so a CC schema drift (shape evolving away from
			// what test/e2e/askuser validated) is traceable instead of
			// silently producing zero cards. Only log input_len — the raw
			// payload may contain user prompt fragments that don't belong
			// in structured logs.
			slog.Debug("extractAskQuestion: input unmarshal failed",
				"err", err, "input_len", len(b.Input))
			return nil
		}
		if len(inp.Questions) == 0 {
			return nil
		}
		items := make([]AskQuestionItem, 0, len(inp.Questions))
		for _, q := range inp.Questions {
			opts := make([]AskQuestionOpt, 0, len(q.Options))
			for _, o := range q.Options {
				opts = append(opts, AskQuestionOpt{Label: o.Label, Description: o.Description})
			}
			items = append(items, AskQuestionItem{
				Question:    q.Question,
				Header:      q.Header,
				MultiSelect: q.MultiSelect,
				Options:     opts,
			})
		}
		return &AskQuestion{ToolUseID: b.ID, Items: items}
	}
	return nil
}

func (p *ClaudeProtocol) HandleEvent(_ io.Writer, _ Event) bool {
	return false
}
