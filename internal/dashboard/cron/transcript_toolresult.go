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
