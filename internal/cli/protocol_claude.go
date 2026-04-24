package cli

import (
	"encoding/json"
	"fmt"
	"io"
)

// ClaudeProtocol implements Protocol for Claude CLI's stream-json format.
type ClaudeProtocol struct {
	// SettingsFile is passed to --settings <file>. When non-empty, standard setting
	// sources are disabled (--setting-sources "") and this file is loaded instead.
	// Use writeClaudeSettingsOverride() to generate a filtered copy of user settings
	// that strips hooks calling back into naozhi.
	SettingsFile string
}

func (p *ClaudeProtocol) Name() string { return "stream-json" }

func (p *ClaudeProtocol) Clone() Protocol { return &ClaudeProtocol{SettingsFile: p.SettingsFile} }

func (p *ClaudeProtocol) BuildArgs(opts SpawnOptions) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--setting-sources", "", // disable standard settings to avoid hook loops
		"--dangerously-skip-permissions",
	}
	if p.SettingsFile != "" {
		args = append(args, "--settings", p.SettingsFile)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

func (p *ClaudeProtocol) Init(_ *JSONRW, _ string) (string, error) {
	return "", nil
}

func (p *ClaudeProtocol) WriteMessage(w io.Writer, text string, images []ImageData) error {
	msg := NewUserMessage(text, images)
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
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

func (p *ClaudeProtocol) ReadEvent(line string) (Event, bool, error) {
	var ev Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return Event{}, false, err
	}
	// Skip hook events
	if ev.Type == "system" && (ev.SubType == "hook_started" || ev.SubType == "hook_response") {
		return Event{}, false, nil
	}
	// Skip control_response — it's a protocol-level ack for our own
	// control_request (interrupt) and carries no user-visible payload.
	// Forwarding it would confuse logEvent / EventEntriesFromEvent.
	if ev.Type == "control_response" {
		return Event{}, false, nil
	}
	return ev, ev.Type == "result", nil
}

func (p *ClaudeProtocol) HandleEvent(_ io.Writer, _ Event) bool {
	return false
}
