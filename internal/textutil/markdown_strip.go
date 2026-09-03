package textutil

import (
	"regexp"
	"strings"
)

// Inline markdown patterns applied per line (outside fenced blocks).
var (
	// ![alt](url) → alt. Runs before the link pattern so the leading '!' is
	// consumed with the image instead of surviving as a stray character.
	mdImageRe = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	// [text](url) → text.
	mdLinkRe = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	// [text](url-that-was-truncated... → text. TruncateRunes may cut a link
	// mid-URL; keeping "[判分](https://ex..." would defeat the whole point.
	mdOpenLinkRe = regexp.MustCompile(`\[([^\]]+)\]\([^)]*$`)
	// Leading list markers: "- ", "* ", "+ ", "1. ", "1) ".
	mdListRe = regexp.MustCompile(`^(?:[-*+]|\d{1,3}[.)])\s+`)
	// Horizontal rules / setext underlines: a line of only -, =, *, _ (3+).
	mdRuleRe  = regexp.MustCompile(`^(?:-{3,}|={3,}|\*{3,}|_{3,})\s*$`)
	mdSpaceRe = regexp.MustCompile(`\s+`)
)

// StripMarkdown removes common Markdown notation from s so the remaining
// plain text can be shown in a single-line UI slot (sidebar preview,
// notification snippet). It is deliberately heuristic: the goal is "no
// visible `##` / `**` / backticks", not a spec-complete CommonMark render.
//
// Handled: ATX headings, blockquote prefixes, list bullets / ordered
// markers, horizontal rules, fenced-code fence lines (the code itself is kept
// verbatim, including for an unclosed trailing fence), inline code
// backticks, emphasis (** __ * ~~ and word-boundary _), links, images.
// Lines are joined with a single space and runs of whitespace collapse, so
// the result never contains a newline.
func StripMarkdown(s string) string {
	if s == "" {
		return ""
	}
	// Fast path: nothing that looks like markdown → only whitespace collapse.
	if !strings.ContainsAny(s, "#*_`[!>~\n-=+") {
		return strings.TrimSpace(s)
	}
	var out []string
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if isFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence {
			if trimmed != "" {
				out = append(out, trimmed)
			}
			continue
		}
		if trimmed == "" || mdRuleRe.MatchString(trimmed) {
			continue
		}
		trimmed = stripLinePrefix(trimmed)
		trimmed = stripInline(trimmed)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return mdSpaceRe.ReplaceAllString(strings.TrimSpace(strings.Join(out, " ")), " ")
}

// isFenceLine reports whether a trimmed line opens or closes a fenced code
// block (``` or ~~~, optionally followed by an info string).
func isFenceLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

// stripLinePrefix peels block-level prefixes off a trimmed line, repeating so
// nested forms ("> ## title", "- > quote", "1. **bold**") all resolve.
func stripLinePrefix(line string) string {
	for {
		switch {
		case strings.HasPrefix(line, ">"):
			line = strings.TrimLeft(line[1:], " \t")
		case strings.HasPrefix(line, "#"):
			i := 0
			for i < len(line) && i < 6 && line[i] == '#' {
				i++
			}
			// A heading needs whitespace (or end) after the hashes; "#tag"
			// and "#123" stay as written.
			if i == len(line) {
				return ""
			}
			if line[i] != ' ' && line[i] != '\t' {
				return line
			}
			line = strings.TrimLeft(line[i:], " \t")
		default:
			if m := mdListRe.FindString(line); m != "" {
				line = line[len(m):]
				continue
			}
			return line
		}
	}
}

// stripInline removes inline markup from a single line.
func stripInline(line string) string {
	line = mdImageRe.ReplaceAllString(line, "$1")
	line = mdLinkRe.ReplaceAllString(line, "$1")
	line = mdOpenLinkRe.ReplaceAllString(line, "$1")
	line = strings.ReplaceAll(line, "`", "")
	line = strings.ReplaceAll(line, "**", "")
	line = strings.ReplaceAll(line, "__", "")
	line = strings.ReplaceAll(line, "~~", "")
	return stripSingleEmphasis(line)
}

// stripSingleEmphasis drops '*' and '_' used as emphasis delimiters while
// keeping them when they are clearly literal: a '*' with whitespace on both
// sides ("5 * 3") and a '_' embedded inside a word (snake_case).
func stripSingleEmphasis(line string) string {
	if !strings.ContainsAny(line, "*_") {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c != '*' && c != '_' {
			b.WriteByte(c)
			continue
		}
		prevSpace := i == 0 || isSpaceByte(line[i-1])
		nextSpace := i+1 == len(line) || isSpaceByte(line[i+1])
		if c == '*' {
			if prevSpace && nextSpace {
				b.WriteByte(c)
			}
			continue
		}
		// '_' is a delimiter only at a word edge.
		if prevSpace || nextSpace || isPunctByte(line[i-1]) || isPunctByte(line[i+1]) {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' }

// isPunctByte covers ASCII punctuation that commonly abuts an emphasis
// delimiter ("_x_," / "(_y_)"); the caller guards index bounds.
func isPunctByte(c byte) bool {
	return strings.IndexByte(".,;:!?()[]{}\"'", c) >= 0
}
