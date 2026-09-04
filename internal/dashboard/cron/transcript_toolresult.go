package cron

import (
	"encoding/json"
	"strings"
)

// toolResultText flattens a tool_result block's content to display text. The
// CLI persists it either as a JSON string or as an array of content blocks;
// text blocks are joined newline-separated, mirroring cli.flattenToolResultRaw,
// and non-text items (tool_reference envelopes) are skipped (#2433).
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

// decodeStringOrBlocks accepts either a JSON string or an array of content
// blocks and returns (string-form, blocks-form); one of the two is empty.
func decodeStringOrBlocks(raw json.RawMessage) (string, []claudeContentBlock) {
	if len(raw) == 0 {
		return "", nil
	}
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
