package cron

import (
	"encoding/json"
	"strings"
)

// toolResultText flattens a tool_result block's content to display text.
// The CLI persists it either as a JSON string or as an array of content
// blocks ([{"type":"text","text":…}, …]). The array form used to be
// dropped (decodeStringOrBlocks handed back blocks the caller discarded),
// leaving Output empty for most tool_result rows (#2433). Text blocks are
// joined newline-separated, mirroring cli.flattenToolResultRaw; non-text
// items (tool_reference envelopes) are skipped.
func toolResultText(raw json.RawMessage) string {
	s, blocks := decodeStringOrBlocks(raw)
	if s != "" || len(blocks) == 0 {
		return s
	}
	var b strings.Builder
	for i := range blocks {
		if blocks[i].Type != "text" || blocks[i].Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(blocks[i].Text)
	}
	return b.String()
}

// decodeStringOrBlocks / toolResultText live together: the latter is the
// only consumer that needs the block form beyond the flatten* callers, and
// keeping both out of transcript.go holds that file under its size baseline.
// decodeStringOrBlocks accepts either a JSON string or an array of
// content blocks and returns (string-form, blocks-form). One of the
// two is empty depending on what the input was.
func decodeStringOrBlocks(raw json.RawMessage) (string, []claudeContentBlock) {
	if len(raw) == 0 {
		return "", nil
	}
	// Strings are a quoted JSON value starting with `"`.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", nil
		}
		return s, nil
	}
	if raw[0] == '[' {
		var blocks []claudeContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return "", nil
		}
		return "", blocks
	}
	// Object — uncommon; ignore.
	return "", nil
}
