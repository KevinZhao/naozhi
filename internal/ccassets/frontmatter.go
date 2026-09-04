package ccassets

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
)

// fmMeta holds the subset of YAML frontmatter the asset browser surfaces.
type fmMeta struct {
	name        string
	description string
}

// maxFrontmatterBytes bounds the search for the closing "---" so an unclosed
// block does not read the whole file.
const maxFrontmatterBytes = 16 << 10

// readFrontmatter reads only the leading YAML frontmatter block of path and
// extracts name/description. No frontmatter → zero fmMeta, nil error.
func readFrontmatter(path string) (fmMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return fmMeta{}, err
	}
	defer f.Close()

	r := bufio.NewReader(io.LimitReader(f, maxFrontmatterBytes))
	first, err := r.ReadString('\n')
	if err != nil && first == "" {
		// Empty file degrades; an unreadable path (e.g. EISDIR for a directory
		// named SKILL.md) propagates so existence-probe callers can skip it.
		if errors.Is(err, io.EOF) {
			return fmMeta{}, nil
		}
		return fmMeta{}, err
	}
	if strings.TrimRight(first, "\r\n") != "---" {
		return fmMeta{}, nil
	}

	var meta fmMeta
	for {
		line, err := r.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "---" {
			break
		}
		if k, v, ok := splitYAMLScalar(trimmed); ok {
			switch k {
			case "name":
				meta.name = v
			case "description":
				meta.description = v
			}
		}
		if err != nil {
			break
		}
	}
	return meta, nil
}

// splitYAMLScalar parses a top-level "key: value" line. Only flat scalar keys
// are handled (nested YAML is ignored). ok=false for blank, indented, or
// comment lines.
func splitYAMLScalar(line string) (key, val string, ok bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
		return "", "", false
	}
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	val = strings.TrimSpace(line[i+1:])
	val = strings.Trim(val, `"'`)
	return key, val, true
}
