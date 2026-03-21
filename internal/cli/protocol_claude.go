package cli

import (
	"encoding/json"
	"io"
)

// ClaudeProtocol implements Protocol for Claude CLI's stream-json format.
type ClaudeProtocol struct{}

func (p *ClaudeProtocol) Name() string { return "stream-json" }

func (p *ClaudeProtocol) BuildArgs(opts SpawnOptions) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--setting-sources", "",
		"--dangerously-skip-permissions",
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

func (p *ClaudeProtocol) WriteMessage(w io.Writer, text string) error {
	msg := NewUserMessage(text)
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func (p *ClaudeProtocol) ReadEvent(line []byte) (Event, bool, error) {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return Event{}, false, err
	}
	// Skip hook events
	if ev.Type == "system" && (ev.SubType == "hook_started" || ev.SubType == "hook_response") {
		return Event{}, false, nil
	}
	return ev, ev.Type == "result", nil
}

func (p *ClaudeProtocol) HandleEvent(_ io.Writer, _ Event) bool {
	return false
}
