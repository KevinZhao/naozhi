// cron_markdown.go: cron helpers that must stay importable without the cron
// package — the IM `/cron` edge (dispatch must not import cron, #1164) and
// cron/sandboxstore (imported by cron) both use them.

package textutil

import "strings"

// MaxCronPromptBytes bounds a cron prompt body (aliased as cron.MaxPromptBytes).
const MaxCronPromptBytes = 8 * 1024

// cronMarkdownPunctReplacer maps `[` `]` `(` `)` to full-width look-alikes.
var cronMarkdownPunctReplacer = strings.NewReplacer(
	"[", "［",
	"]", "］",
	"(", "（",
	")", "）",
)

// EscapeCronMarkdownPunct replaces the markdown link-syntax characters
// `[`, `]`, `(`, `)` with full-width look-alikes so an attacker-controlled
// cron Title or result body cannot smuggle `[text](url)` clickable links into
// an IM notice. Allocation-free on the common clean case; idempotent.
func EscapeCronMarkdownPunct(s string) string {
	if !strings.ContainsAny(s, "[]()") {
		return s
	}
	return cronMarkdownPunctReplacer.Replace(s)
}
