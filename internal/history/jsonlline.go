package history

import (
	"bufio"
	"errors"
)

// ReadJSONLLine reads one '\n'-terminated line from r. A normal line is
// returned without its '\n' / "\r\n" (the slice is only valid until the next
// read); a line whose content exceeds maxLine bytes is drained and reported as
// oversized=true with nil bytes, retaining at most ~maxLine bytes of memory.
// At EOF the final unterminated line (if any) is returned with io.EOF. Any
// other non-nil err is a real read failure.
//
// Unlike bufio.Scanner, an oversized line (the CLIs inline base64 images and
// tool results of several MiB on one NDJSON line) skips just that line rather
// than aborting the whole read with "token too long".
func ReadJSONLLine(r *bufio.Reader, maxLine int) (line []byte, oversized bool, err error) {
	frag, err := r.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		// Fast path: a complete line fit in the reader's buffer (or EOF / real
		// error). frag aliases the internal buffer; hand it over without copying.
		return trimNewline(frag), false, err
	}

	// Slow path: the line exceeds the reader buffer. `content` counts bytes
	// excluding the terminator and buf only grows while under the cap, so an
	// oversized line is drained, not retained. Mid-line fragments never
	// contain '\n'; only the final one carries it.
	content := len(frag)
	buf := append([]byte(nil), frag...)
	for {
		frag, err = r.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		if full {
			content += len(frag)
		} else {
			content += len(trimNewline(frag))
		}
		if content <= maxLine {
			buf = append(buf, frag...)
		}
		if !full {
			break
		}
		if content > maxLine {
			// Drain the rest of the line without retaining it; err ends as nil
			// or io.EOF, both handled by the caller.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			return nil, true, err
		}
	}
	if content > maxLine {
		return nil, true, err
	}
	return trimNewline(buf), false, err
}

// trimNewline drops a single trailing '\n' (and a preceding '\r') so callers
// see the same line bytes a bufio.Scanner would have produced.
func trimNewline(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}
