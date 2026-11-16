package wireup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	// maxTranscriptLine caps one transcript line read; a longer one (a huge
	// tool result) is skipped.
	maxTranscriptLine = 16 << 20
	// maxCachedTranscripts bounds TranscriptDirs; past it the cache starts
	// over, each transcript then read whole once more.
	maxCachedTranscripts = 4096
)

// TranscriptDirs reads the directories transcripts went into, each file
// from where its last read stopped: reap's InUse reads every live session's
// transcripts on each worktree check, and they only grow. Safe for
// concurrent use; a nil *TranscriptDirs reads every file whole.
type TranscriptDirs struct {
	mu    sync.Mutex
	files map[string]*scanned
}

// NewTranscriptDirs returns an empty TranscriptDirs.
func NewTranscriptDirs() *TranscriptDirs {
	return &TranscriptDirs{files: map[string]*scanned{}}
}

// scanned is what was read of one transcript: up to off, the end of its
// last complete line, so a line being written is read again whole.
type scanned struct {
	off  int64
	cmds bool
	dirs []string
	seen map[string]bool
}

// read is the distinct directories path's lines went into, in the order
// first seen: each line's cwd, and with cmds the directories its Bash tool
// calls cd into.
func (c *TranscriptDirs) read(path string, cmds bool) []string {
	if c == nil {
		s := &scanned{cmds: cmds, seen: map[string]bool{}}
		s.scan(path)
		return s.dirs
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.files[path]
	if s == nil || s.cmds != cmds {
		if len(c.files) >= maxCachedTranscripts {
			clear(c.files)
		}
		s = &scanned{cmds: cmds, seen: map[string]bool{}}
		c.files[path] = s
	}
	if !s.scan(path) {
		delete(c.files, path)
		return nil
	}
	return s.dirs
}

// scan reads path from s.off; false when path is not a readable file.
func (s *scanned) scan(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if fi.Size() < s.off {
		// Rewritten shorter: what was read may no longer be there.
		*s = scanned{cmds: s.cmds, seen: map[string]bool{}}
	}
	if fi.Size() == s.off {
		return true
	}
	if _, err := f.Seek(s.off, io.SeekStart); err != nil {
		return true
	}
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, n, complete := readLine(br, maxTranscriptLine)
		if !complete {
			return true
		}
		s.off += n
		if line != nil {
			s.line(line)
		}
	}
}

// readLine reads one line ending in '\n': n is its length, line nil when it
// is longer than limit; complete is false at the end of the file before a
// '\n' (a line still being written, or none).
func readLine(br *bufio.Reader, limit int) (line []byte, n int64, complete bool) {
	var buf []byte
	over := false
	for {
		frag, err := br.ReadSlice('\n')
		n += int64(len(frag))
		if !over {
			if len(buf)+len(frag) > limit {
				over, buf = true, nil
			} else {
				buf = append(buf, frag...)
			}
		}
		switch {
		case err == nil:
			if over {
				return nil, n, true
			}
			return buf, n, true
		case errors.Is(err, bufio.ErrBufferFull):
		default:
			return nil, n, false
		}
	}
}

var (
	cwdKey     = []byte(`"cwd":`)
	toolUseKey = []byte(`"tool_use"`)
)

func (s *scanned) line(line []byte) {
	hasCwd := bytes.Contains(line, cwdKey)
	hasTool := s.cmds && bytes.Contains(line, toolUseKey)
	if !hasCwd && !hasTool {
		return
	}
	var v struct {
		Cwd     string `json:"cwd"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &v) != nil {
		return
	}
	if filepath.IsAbs(v.Cwd) {
		s.add(v.Cwd)
	}
	if !hasTool || len(v.Message.Content) == 0 || v.Message.Content[0] != '[' {
		return
	}
	var blocks []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Input struct {
			Command string `json:"command"`
		} `json:"input"`
	}
	if json.Unmarshal(v.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && b.Name == "Bash" {
			for _, d := range commandDirs(b.Input.Command) {
				s.add(d)
			}
		}
	}
}

func (s *scanned) add(d string) {
	if !s.seen[d] {
		s.seen[d] = true
		s.dirs = append(s.dirs, d)
	}
}

// cdArg matches a shell command's cd, pushd or git -C and its directory.
var cdArg = regexp.MustCompile(`(?:^|[\s;&|(])(?:cd|pushd|git\s+-C)\s+("[^"]*"|'[^']*'|[^\s;&|()<>]+)`)

// commandDirs are the absolute directories a shell command cd's into (or
// runs git in), ~ and $HOME expanded; relative ones and those naming other
// variables are left out, the shell's directory at that point not known.
func commandDirs(cmd string) []string {
	if !strings.Contains(cmd, "cd") && !strings.Contains(cmd, "pushd") && !strings.Contains(cmd, "-C") {
		return nil
	}
	home, _ := os.UserHomeDir()
	var out []string
	for _, m := range cdArg.FindAllStringSubmatch(cmd, -1) {
		arg := m[1]
		if len(arg) >= 2 && (arg[0] == '"' || arg[0] == '\'') {
			arg = arg[1 : len(arg)-1]
		}
		if home != "" {
			for _, pre := range []string{"~", "$HOME", "${HOME}"} {
				if rest, ok := strings.CutPrefix(arg, pre); ok && (rest == "" || rest[0] == '/') {
					arg = home + rest
					break
				}
			}
		}
		if !filepath.IsAbs(arg) || strings.ContainsAny(arg, "$`*?") {
			continue
		}
		out = append(out, filepath.Clean(arg))
	}
	return out
}
