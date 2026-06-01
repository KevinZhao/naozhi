package ccassets

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// fmMeta holds the subset of YAML frontmatter the asset browser surfaces.
type fmMeta struct {
	name        string
	description string
}

// maxFrontmatterBytes bounds how far into a file we read looking for the
// closing "---". Frontmatter is tiny; this guards against a file whose first
// line is "---" but which never closes it (we'd otherwise read the whole
// file). 16 KiB is generous for any real frontmatter block.
const maxFrontmatterBytes = 16 << 10

// readFrontmatter reads only the leading YAML frontmatter block of path
// (between the first two "---" lines) and extracts name/description. It does
// NOT read the markdown body (that is served lazily by ReadRaw). A file with
// no frontmatter, or a parse miss, returns a zero fmMeta and nil error — the
// caller degrades gracefully (RFC §1.2-6).
func readFrontmatter(path string) (fmMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return fmMeta{}, err
	}
	defer f.Close()

	r := bufio.NewReader(io.LimitReader(f, maxFrontmatterBytes))
	first, err := r.ReadString('\n')
	if err != nil && first == "" {
		return fmMeta{}, nil
	}
	if strings.TrimRight(first, "\r\n") != "---" {
		// No frontmatter — degrade.
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
			// EOF before closing "---": treat what we parsed as best-effort.
			break
		}
	}
	return meta, nil
}

// splitYAMLScalar parses a top-level "key: value" line. It deliberately only
// handles flat scalar keys (name/description) — nested YAML (metadata:) is
// ignored, which is all the asset browser needs. Returns ok=false for blank,
// indented, or comment lines.
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
