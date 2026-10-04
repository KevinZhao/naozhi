package replyfmt

import (
	"strings"
	"unicode/utf8"
)

// maxReopenInfoRunes caps the info string ("go", "python title=x") copied onto
// a reopened fence; a longer one is dropped and the fence reopens bare.
const maxReopenInfoRunes = 32

// SplitText splits text into chunks of at most maxRunes runes, preferring a
// newline in the second half of each window. runeCount must be
// utf8.RuneCountInString(text); a wrong value only affects the single-chunk
// fast path.
//
// A cut inside a ``` or ~~~ code fence closes the fence at the end of that
// chunk and reopens it, info string included, at the start of the next, so each
// message renders as code on its own. The close and reopen lines count against
// maxRunes. Text with no fence marker never gets a synthesized line.
func SplitText(text string, maxRunes, runeCount int) []string {
	if runeCount <= maxRunes {
		return []string{text}
	}
	if maxRunes < 1 {
		maxRunes = 1
	}
	if !strings.Contains(text, "```") && !strings.Contains(text, "~~~") {
		var chunks []string
		for text != "" {
			end := cutIndex(text, maxRunes)
			chunks = append(chunks, text[:end])
			text = text[end:]
		}
		return chunks
	}
	parts := splitFenced(text, maxRunes)
	chunks := make([]string, len(parts))
	for i, p := range parts {
		chunks[i] = p.head + p.body + p.tail
	}
	return chunks
}

// cutIndex returns the byte length of the next chunk of text for a window of w
// runes: all of text when it fits, else the window shortened to just after its
// last newline when that newline sits past the window's rune midpoint.
func cutIndex(text string, w int) int {
	end := 0
	for n := 0; n < w && end < len(text); n++ {
		_, size := utf8.DecodeRuneInString(text[end:])
		end += size
	}
	if end == len(text) {
		return end
	}
	// Measured in runes, not bytes, so upperBoundChunks' minimum chunk holds
	// for text that mixes 1-byte and 3-byte runes.
	if idx := strings.LastIndexByte(text[:end], '\n'); idx >= 0 && utf8.RuneCountInString(text[:idx]) > w/2 {
		return idx + 1
	}
	return end
}

// fence is a code fence open at some point in the text.
type fence struct {
	indent string // leading spaces/tabs of the opener line
	char   byte   // '`' or '~'
	run    int    // marker length; a closer needs at least this many
	opener string // the line that reopens it, without the newline
}

// closer returns the line that closes f.
func (f *fence) closer() string { return f.indent + strings.Repeat(string(f.char), f.run) }

// splitPart is one chunk: body is a slice of the input, head and tail are the
// synthesized reopen and close lines (empty when none). plain marks a chunk
// cut without fence repair because the overhead did not fit.
type splitPart struct {
	head, body, tail string
	plain            bool
}

// splitFenced is SplitText for text containing a fence marker. The reopen and
// close overhead of one chunk is capped at maxRunes/4 so every chunk but the
// last still consumes more than 3/8 of maxRunes (upperBoundChunks relies on
// it); a chunk whose overhead would exceed that is cut plainly instead.
func splitFenced(text string, maxRunes int) []splitPart {
	var parts []splitPart
	var open *fence // fence open where the remaining text starts
	budget := maxRunes / 4
	for text != "" {
		head := ""
		if open != nil {
			head = open.opener + "\n"
		}
		headRunes := utf8.RuneCountInString(head)
		end, tail := -1, ""
		if headRunes <= budget {
			end, tail = cutFenced(text, open, maxRunes-headRunes, budget-headRunes)
		}
		plain := end < 0
		if plain {
			head, end = "", cutIndex(text, maxRunes)
		}
		body := text[:end]
		open = scanFences(body, open)
		parts = append(parts, splitPart{head: head, body: body, tail: tail, plain: plain})
		text = text[end:]
	}
	return parts
}

// cutFenced picks the cut for a chunk with w runes left after the reopen line,
// reserving room for the closing line when the cut lands inside a fence. A cut
// just before the input's own closing line moves past it when that fits. It
// returns end -1 when the synthesized closing line would exceed maxTail runes.
func cutFenced(text string, open *fence, w, maxTail int) (end int, tail string) {
	reserve := 0
	for {
		end = cutIndex(text, w-reserve)
		if end == len(text) {
			return end, ""
		}
		f := scanFences(text[:end], open)
		if f == nil {
			return end, ""
		}
		if text[end-1] == '\n' {
			// An opener on the chunk's last line would leave an empty block
			// here; cut before it instead when the chunk keeps over half of
			// the narrowest window.
			start := strings.LastIndexByte(text[:end-1], '\n') + 1
			if start > 0 && scanFences(text[:start], open) == nil && utf8.RuneCountInString(text[:start]) > (w-maxTail)/2 {
				return start, ""
			}
			// A closing line just past the cut would open the next chunk with
			// an empty reopened block; take it into this chunk when it fits.
			if line, _, _ := strings.Cut(text[end:], "\n"); stepFence(line, f) == nil {
				for _, e := range []int{end + len(line) + 1, end + len(line)} {
					if e <= len(text) && utf8.RuneCountInString(text[:e]) <= w {
						return e, ""
					}
				}
			}
		}
		tail = f.closer()
		if text[end-1] != '\n' {
			tail = "\n" + tail
		}
		need := len(tail) // indent and marker are ASCII
		if need <= reserve {
			return end, tail
		}
		if need > maxTail {
			return -1, ""
		}
		reserve = need
	}
}

// scanFences returns the fence open after text given the one open before it.
// Every newline-separated segment counts as a line, the partial ones at either
// end included, because each chunk is rendered as a message of its own.
func scanFences(text string, open *fence) *fence {
	for text != "" {
		line := text
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			line, text = text[:i], text[i+1:]
		} else {
			text = ""
		}
		open = stepFence(line, open)
	}
	return open
}

// stepFence applies one line to the fence state, following CommonMark's
// fenced-code rules except that any indentation is accepted: chat renderers
// show fences nested in list items, which sit four or more spaces in.
func stepFence(line string, open *fence) *fence {
	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return open
	}
	c := trimmed[0]
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	if n < 3 {
		return open
	}
	info := strings.TrimRight(trimmed[n:], " \t\r")
	if open != nil {
		if c == open.char && n >= open.run && info == "" {
			return nil
		}
		return open
	}
	if c == '`' && strings.IndexByte(info, '`') >= 0 {
		return open // inline code such as ```x```, not an opener
	}
	indent := line[:len(line)-len(trimmed)]
	f := &fence{indent: indent, char: c, run: n, opener: indent + trimmed[:n]}
	if info != "" && utf8.RuneCountInString(info) <= maxReopenInfoRunes {
		f.opener += info
	}
	return f
}
