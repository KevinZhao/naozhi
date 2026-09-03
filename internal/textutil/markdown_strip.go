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
// backticks, ~~strike~~, paired emphasis (** __ * _ — a delimiter only counts
// when its opener is preceded by start-of-line / whitespace and its closer is
// followed by end / whitespace / punctuation, so "5*3", "a/*.go", snake_case
// and "__init__.py" stay literal), links, images. Lines are joined with a
// single space and runs of whitespace collapse, so the result never contains
// a newline. If stripping would leave nothing (e.g. "###"), the
// whitespace-collapsed input is returned instead of an empty preview.
func StripMarkdown(s string) string {
	if s == "" {
		return ""
	}
	// Fast path: nothing that looks like markdown → only whitespace collapse.
	if !strings.ContainsAny(s, "#*_`[!>~\n-=+") {
		return strings.TrimSpace(s)
	}
	out := stripMarkdownLines(s)
	if out == "" {
		return collapseSpace(s)
	}
	return out
}

func collapseSpace(s string) string {
	return mdSpaceRe.ReplaceAllString(strings.TrimSpace(s), " ")
}

func stripMarkdownLines(s string) string {
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
	return collapseSpace(strings.Join(out, " "))
}

// isFenceLine reports whether a trimmed line opens or closes a fenced code
// block: "```" (any length) or exactly "~~~", each optionally followed by an
// info string. A longer tilde run ("~~~~") is not a fence.
func isFenceLine(trimmed string) bool {
	if strings.HasPrefix(trimmed, "```") {
		return true
	}
	return strings.HasPrefix(trimmed, "~~~") && (len(trimmed) == 3 || trimmed[3] != '~')
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
	line = strings.ReplaceAll(line, "~~", "")
	return stripEmphasisPairs(line)
}

// stripEmphasisPairs drops matched emphasis delimiters (** __ * _) and keeps
// every unmatched or literal occurrence. A run of 1–2 identical delimiter
// characters opens emphasis only when preceded by start-of-line, whitespace
// or an opening bracket/quote and followed by a non-space; the closer is a
// run of the same character and length that is preceded by a non-space and
// followed by end-of-line, whitespace or punctuation (ASCII or CJK).
// Underscore closers additionally reject a following ".ext" so Python-style
// "__init__.py" survives.
func stripEmphasisPairs(line string) string {
	if !strings.ContainsAny(line, "*_") {
		return line
	}
	rs := []rune(line)
	n := len(rs)
	drop := make([]bool, n)
	for i := 0; i < n; {
		c := rs[i]
		if c != '*' && c != '_' {
			i++
			continue
		}
		j := i
		for j < n && rs[j] == c {
			j++
		}
		runLen := j - i
		if runLen > 2 || !emphasisOpenerOK(rs, i, j) {
			i = j
			continue
		}
		k := findEmphasisCloser(rs, j, c, runLen)
		if k < 0 {
			i = j
			continue
		}
		for x := i; x < j; x++ {
			drop[x] = true
		}
		for x := k; x < k+runLen; x++ {
			drop[x] = true
		}
		i = j
	}
	var b strings.Builder
	b.Grow(len(line))
	for i, r := range rs {
		if !drop[i] {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// findEmphasisCloser returns the index of the first valid closing run of
// character c with length runLen at or after from, or -1.
func findEmphasisCloser(rs []rune, from int, c rune, runLen int) int {
	n := len(rs)
	for k := from; k+runLen <= n; k++ {
		if rs[k] != c {
			continue
		}
		e := k
		for e < n && rs[e] == c {
			e++
		}
		if e-k == runLen && k > from && emphasisCloserOK(rs, k, e, c) {
			return k
		}
		k = e - 1
	}
	return -1
}

func emphasisOpenerOK(rs []rune, i, j int) bool {
	if j >= len(rs) || isSpaceRune(rs[j]) {
		return false
	}
	return i == 0 || isSpaceRune(rs[i-1]) || isOpenPunct(rs[i-1])
}

func emphasisCloserOK(rs []rune, k, e int, c rune) bool {
	if isSpaceRune(rs[k-1]) {
		return false
	}
	if e == len(rs) {
		return true
	}
	next := rs[e]
	if isSpaceRune(next) {
		return true
	}
	if !isClosePunct(next) {
		return false
	}
	// "__init__.py": a dot that starts a file extension is not a closer.
	if c == '_' && next == '.' && e+1 < len(rs) && isAlnumRune(rs[e+1]) {
		return false
	}
	return true
}

func isSpaceRune(r rune) bool { return r == ' ' || r == '\t' }

func isAlnumRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// isOpenPunct / isClosePunct cover the ASCII and CJK full-width punctuation
// that commonly abuts an emphasis delimiter: "(*x*)" / "*强调*，".
func isOpenPunct(r rune) bool {
	return strings.ContainsRune("([{\"'（【「『“‘《", r)
}

func isClosePunct(r rune) bool {
	return strings.ContainsRune(".,;:!?)]}\"'，。；：！？）】」』”’》、", r)
}
