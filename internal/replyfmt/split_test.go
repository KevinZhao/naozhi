package replyfmt

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

func split(text string, maxRunes int) []string {
	return SplitText(text, maxRunes, utf8.RuneCountInString(text))
}

func TestSplitText_Fences(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		text     string
		maxRunes int
		want     []string
	}{
		{
			name:     "cut inside a fence closes and reopens it",
			text:     "```go\nl1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\n```\n",
			maxRunes: 24,
			want: []string{
				"```go\nl1\nl2\nl3\nl4\nl5\n```",
				"```go\nl6\nl7\nl8\nl9\n```\n",
			},
		},
		{
			name:     "tilde fence",
			text:     "~~~\nl1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\n~~~\n",
			maxRunes: 20,
			want: []string{
				"~~~\nl1\nl2\nl3\nl4\n~~~",
				"~~~\nl5\nl6\nl7\nl8\n~~~\n",
			},
		},
		{
			name:     "inner triple backticks do not close a four-backtick fence",
			text:     "````md\n```\nin1\nin2\n```\nin3\nin4\n````\n",
			maxRunes: 28,
			want: []string{
				"````md\n```\nin1\nin2\n```\n````",
				"````md\nin3\nin4\n````\n",
			},
		},
		{
			name:     "cut right after a closing fence adds nothing",
			text:     "```\nabcdefg\n```\nxxxxxxxxxxxxxxx\n",
			maxRunes: 16,
			want: []string{
				"```\nabcdefg\n```\n",
				"xxxxxxxxxxxxxxx\n",
			},
		},
		{
			name:     "opener on the last line moves to the next chunk",
			text:     "aaaaaaaaaaaaaaaa\n```\ncode\n```\n",
			maxRunes: 24,
			want: []string{
				"aaaaaaaaaaaaaaaa\n",
				"```\ncode\n```\n",
			},
		},
		{
			name:     "opener stays when moving it would leave under half a window",
			text:     strings.Repeat("a", 25) + "\n```" + strings.Repeat("i", 12) + "\n" + strings.Repeat("c", 40) + "\n```\n",
			maxRunes: 80,
			want: []string{
				strings.Repeat("a", 25) + "\n```" + strings.Repeat("i", 12) + "\n```",
				"```" + strings.Repeat("i", 12) + "\n" + strings.Repeat("c", 40) + "\n```\n",
			},
		},
		{
			name:     "unterminated fence gets no closer on the last chunk",
			text:     "```py\nl1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\n",
			maxRunes: 24,
			want: []string{
				"```py\nl1\nl2\nl3\nl4\nl5\n```",
				"```py\nl6\nl7\nl8\n",
			},
		},
		{
			name:     "a long info string is dropped on reopen",
			text:     "```" + strings.Repeat("i", 33) + "\n" + strings.Repeat("c\n", 30) + "```\n",
			maxRunes: 60,
			want: []string{
				"```" + strings.Repeat("i", 33) + "\n" + strings.Repeat("c\n", 10) + "```",
				"```\n" + strings.Repeat("c\n", 20) + "```\n",
			},
		},
		{
			name:     "indented fence reopens and closes at its indentation",
			text:     "- step\n    ```sh\n    a\n    b\n    c\n    d\n    ```\n",
			maxRunes: 40,
			want: []string{
				"- step\n    ```sh\n    a\n    b\n    ```",
				"    ```sh\n    c\n    d\n    ```\n",
			},
		},
		{
			name:     "a mid-line cut inside a fence closes on a new line",
			text:     "```\n" + strings.Repeat("x", 70) + "\n```\n",
			maxRunes: 40,
			want: []string{
				"```\n" + strings.Repeat("x", 32) + "\n```",
				"```\n" + strings.Repeat("x", 32) + "\n```",
				"```\n" + strings.Repeat("x", 6) + "\n```\n",
			},
		},
		{
			name:     "inline triple backticks are not an opener",
			text:     "use ```x``` here\n```x``` again\nand more text\n",
			maxRunes: 20,
			want: []string{
				"use ```x``` here\n",
				"```x``` again\n",
				"and more text\n",
			},
		},
		{
			name:     "overhead over a quarter of the budget falls back to a plain cut",
			text:     "```\n" + strings.Repeat("y", 20),
			maxRunes: 12,
			want: []string{
				"```\nyyyyyyyy",
				"yyyyyyyyyyyy",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := split(c.text, c.maxRunes)
			if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
				t.Fatalf("SplitText(%q, %d)\n got %q\nwant %q", c.text, c.maxRunes, got, c.want)
			}
			checkSplit(t, c.text, c.maxRunes)
		})
	}
}

// legacySplit is the splitter as it was before fences were handled, kept as the
// reference for fence-free ASCII text.
func legacySplit(text string, maxRunes int) []string {
	if utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	for text != "" {
		end, count := 0, 0
		for count < maxRunes && end < len(text) {
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
			count++
		}
		if end == len(text) {
			chunks = append(chunks, text)
			break
		}
		if idx := strings.LastIndex(text[:end], "\n"); idx > end/2 {
			end = idx + 1
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

func TestSplitText_FenceFreeASCIIUnchanged(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 2000; i++ {
		var b strings.Builder
		for n := r.IntN(400); n > 0; n-- {
			if r.IntN(12) == 0 {
				b.WriteByte('\n')
			} else {
				b.WriteByte(byte('a' + r.IntN(26)))
			}
		}
		text, maxRunes := b.String(), 1+r.IntN(80)
		got, want := split(text, maxRunes), legacySplit(text, maxRunes)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("SplitText(%q, %d) = %q, legacy %q", text, maxRunes, got, want)
		}
	}
}

// TestSplitText_MidpointCountsRunes: a newline past the byte midpoint but before
// the rune midpoint (3-byte runes first, ASCII after) must not shorten the
// chunk; a byte midpoint lets chunks drop to a fifth of the window, below the
// minimum upperBoundChunks assumes.
func TestSplitText_MidpointCountsRunes(t *testing.T) {
	t.Parallel()
	text := strings.Repeat(strings.Repeat("中", 6)+"\n"+strings.Repeat("a", 13), 5)
	got := split(text, 20)
	if want := 5; len(got) != want {
		t.Fatalf("got %d chunks %q, want %d full windows", len(got), got, want)
	}
	checkSplit(t, text, 20)
}

// TestSplitText_FenceRepairChunkFloor drives every chunk to the shortest that
// fence repair allows: a 16-rune reopen plus a 3-rune close leave a 61-rune
// window, and 32-rune lines make each cut land just past its midpoint. Twelve
// such chunks of 404 runes at width 80 exceed a ceil(width/2) floor's bound of
// 11, which is why upperBoundChunks assumes ceil(3·width/8).
func TestSplitText_FenceRepairChunkFloor(t *testing.T) {
	t.Parallel()
	opener := "```" + strings.Repeat("i", 12)
	line := strings.Repeat("x", 31) + "\n"
	text := opener + "\n" + strings.Repeat(line, 12) + "```\n"
	got := split(text, 80)
	if len(got) != 12 {
		t.Fatalf("got %d chunks, want 12: %q", len(got), got)
	}
	for i, c := range got[:11] {
		if want := opener + "\n" + line + "```"; c != want {
			t.Fatalf("chunk %d = %q, want %q", i, c, want)
		}
	}
	checkSplit(t, text, 80)
}

// genMarkdown builds chat-reply-shaped text: prose and CJK lines, fences of
// both kinds with info strings and indentation, nested-looking marker lines,
// and an occasional very long line.
func genMarkdown(r *rand.Rand, lines, maxLine int) string {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		switch r.IntN(10) {
		case 0, 1:
			b.WriteString(strings.Repeat(" ", r.IntN(5)))
			b.WriteString(strings.Repeat(string("`~"[r.IntN(2)]), 3+r.IntN(3)))
			b.WriteString([]string{"", "go", "python", "sh title=x", "md"}[r.IntN(5)])
		case 2:
			b.WriteString(strings.Repeat("中文", 1+r.IntN(maxLine/2+1)))
		case 3:
			b.WriteString("inline ```x``` code")
		default:
			for n := r.IntN(maxLine); n > 0; n-- {
				b.WriteByte(byte('a' + r.IntN(26)))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func TestSplitText_Properties(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 3000; i++ {
		text := genMarkdown(r, 1+r.IntN(60), 1+r.IntN(120))
		maxRunes := 8 + r.IntN(300)
		if i%10 == 0 {
			maxRunes = 8 + r.IntN(3993)
		}
		checkSplit(t, text, maxRunes)
		if t.Failed() {
			t.Fatalf("input %q maxRunes %d", text, maxRunes)
		}
	}
}

// TestSplitText_ShortLinesBalanced: when every line fits well inside the window
// all cuts land on line boundaries, so a fence-balanced input yields chunks
// that are each balanced, the last included.
func TestSplitText_ShortLinesBalanced(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < 2000; i++ {
		text := genMarkdown(r, 1+r.IntN(80), 16)
		if f := scanFences(text, nil); f != nil {
			text += f.closer() + "\n"
		}
		maxRunes := 120 + r.IntN(480)
		for j, c := range split(text, maxRunes) {
			if f := scanFences(c, nil); f != nil {
				t.Fatalf("chunk %d leaves %q open at maxRunes %d:\n%q", j, f.opener, maxRunes, c)
			}
		}
	}
}

func TestSplitText_PageSuffixFits(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(7, 8))
	for i := 0; i < 3000; i++ {
		text := genMarkdown(r, 1+r.IntN(200), 1+r.IntN(60))
		maxLen := 9 + r.IntN(200)
		n := utf8.RuneCountInString(text)
		splitLen, suppress := ReserveForPageSuffix(maxLen, n)
		chunks := SplitText(text, splitLen, n)
		if suppress || len(chunks) < 2 {
			continue
		}
		for j, c := range chunks {
			if got := utf8.RuneCountInString(c + PageSuffix(j+1, len(chunks))); got > maxLen {
				t.Fatalf("chunk %d/%d plus suffix is %d runes > maxLen %d (splitLen %d, %d runes)",
					j+1, len(chunks), got, maxLen, splitLen, n)
			}
		}
	}
}

func FuzzSplitText(f *testing.F) {
	f.Add("```go\nfunc main() {}\n```\nafter\n", uint16(10))
	f.Add("~~~\n````\n~~~~\nx\n", uint16(0))
	f.Add("    ```sh title=a\n中文中文\n```\n", uint16(5))
	f.Add(strings.Repeat("a\n```\n", 40), uint16(30))
	f.Add("\xff```\n\x80\x80\n", uint16(1))
	f.Fuzz(func(t *testing.T, text string, m uint16) {
		checkSplit(t, text, 8+int(m)%3993)
	})
}

// checkSplit asserts SplitText's contract for one input: chunks within
// maxRunes, no more of them than upperBoundChunks, the input recovered by
// dropping the synthesized lines, and each repaired chunk but the last
// fence-balanced.
func checkSplit(t *testing.T, text string, maxRunes int) {
	t.Helper()
	n := utf8.RuneCountInString(text)
	chunks := SplitText(text, maxRunes, n)
	for i, c := range chunks {
		if got := utf8.RuneCountInString(c); got > maxRunes {
			t.Errorf("chunk %d is %d runes > %d: %q", i, got, maxRunes, c)
		}
	}
	if n > maxRunes {
		if bound := upperBoundChunks(n, maxRunes); len(chunks) > bound {
			t.Errorf("%d chunks > upperBoundChunks(%d, %d) = %d", len(chunks), n, maxRunes, bound)
		}
	}
	if n <= maxRunes || (!strings.Contains(text, "```") && !strings.Contains(text, "~~~")) {
		if strings.Join(chunks, "") != text {
			t.Errorf("fence-free chunks do not concatenate to the input")
		}
		return
	}
	parts := splitFenced(text, maxRunes)
	if len(parts) != len(chunks) {
		t.Fatalf("splitFenced gave %d parts, SplitText %d chunks", len(parts), len(chunks))
	}
	var bodies strings.Builder
	var open *fence
	for i, p := range parts {
		if p.head+p.body+p.tail != chunks[i] {
			t.Errorf("chunk %d is not head+body+tail", i)
		}
		if p.body == "" {
			t.Errorf("part %d has an empty body", i)
		}
		bodies.WriteString(p.body)
		if !p.plain {
			if open != nil && p.head != open.opener+"\n" {
				t.Errorf("part %d head %q, want the reopened %q", i, p.head, open.opener)
			}
			if i < len(parts)-1 && scanFences(chunks[i], nil) != nil {
				t.Errorf("chunk %d is not fence-balanced: %q", i, chunks[i])
			}
		}
		open = scanFences(p.body, open)
	}
	if bodies.String() != text {
		t.Errorf("bodies do not concatenate to the input")
	}
}
