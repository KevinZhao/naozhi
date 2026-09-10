// Package claudefs owns naozhi's knowledge of the Claude CLI's on-disk layout:
// where a session's transcript lives under ~/.claude/projects, how a CWD is
// encoded into a project directory name, and what a session ID may look like.
// naozhi reads these files but does not own them, so every rule here is a
// reproduction of the CLI's behaviour, verified against a specific CLI version
// and pinned by tests. F2 (#2643).
//
// # Why this is a package
//
// The knowledge was spread across five: discovery encoded the slug and joined
// the path, dashboard/cron re-joined it (importing discovery for that one
// symbol), ccassets and session each wrapped the slug function, and the
// `filepath.Join(claudeDir, "projects", slug, id+".jsonl")` expression appeared
// seven times. The slug encoding in particular is security-relevant — it turns
// operator-persisted state (cron_jobs.json, sessions-index.json) into a
// filesystem path, and #465 was a control-byte injection into exactly that step
// — so it must have one implementation, not one plus four wrappers.
//
// # Scope
//
// Paths and identifiers only. Reading a transcript belongs to the packages that
// know what they want out of it: there is exactly ONE reverse reader in the tree
// (discovery/history_tail.go) and consolidating it was explicitly ruled out of
// F2, as were the kiro transcript reader and naozhi's own event-log index, which
// are different formats that happen to also be JSONL.
package claudefs

import (
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// projectsDirName is the directory under claudeDir holding one subdirectory per
// project.
const projectsDirName = "projects"

// sessionsIndexName is the per-project sidecar the CLI writes alongside the
// transcripts.
const sessionsIndexName = "sessions-index.json"

// ProjectsRoot is <claudeDir>/projects. Empty claudeDir yields "" so callers
// that run without a resolvable ~/.claude degrade quietly.
func ProjectsRoot(claudeDir string) string {
	if claudeDir == "" {
		return ""
	}
	return filepath.Join(claudeDir, projectsDirName)
}

// ProjectDir is the directory holding one project's transcripts.
func ProjectDir(claudeDir, cwd string) string {
	root := ProjectsRoot(claudeDir)
	if root == "" {
		return ""
	}
	return filepath.Join(root, ProjectSlug(cwd))
}

// SessionJSONL is a session's transcript path. This replaced seven copies of the
// same Join; the callers that must ALSO defend against escaping the projects
// root (a workspace path arriving from persisted state) still do so after
// building it — this function encodes the layout, it does not authorise the
// read.
func SessionJSONL(claudeDir, cwd, sessionID string) string {
	dir := ProjectDir(claudeDir, cwd)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, sessionID+".jsonl")
}

// SessionsIndexPath is a project's sessions-index.json sidecar.
func SessionsIndexPath(claudeDir, cwd string) string {
	dir := ProjectDir(claudeDir, cwd)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, sessionsIndexName)
}

// slugMaxLen mirrors the Claude CLI's cap on an encoded project directory
// name: beyond it the CLI truncates and appends "-" + a base36 hash of the
// original path (verified against CLI 2.1.219 with a 40-segment CWD).
const slugMaxLen = 200

// ProjectSlug converts a CWD path to the Claude project directory name,
// e.g. "/home/user/workspace/foo" -> "-home-user-workspace-foo"; it is the
// single source of truth for the scheme. Every character outside [A-Za-z0-9]
// becomes '-' per UTF-16 code unit (see EncodeSegment). Control bytes (< 0x20)
// are stripped first so hand-edited persisted state (cron_jobs.json,
// sessions-index.json) with embedded \t/\n cannot steer the encoded path onto
// an attacker-prepared directory (#465).
func ProjectSlug(cwd string) string {
	if hasControlByte(cwd) {
		cwd = stripControlBytes(cwd)
	}
	slug := EncodeSegment(cwd)
	if len(slug) <= slugMaxLen {
		return slug
	}
	return slug[:slugMaxLen] + "-" + slugHash(cwd)
}

// EncodeSegment replaces everything outside [A-Za-z0-9] with '-',
// mirroring the CLI's `replace(/[^a-zA-Z0-9]/g, "-")` per UTF-16 code unit: a
// BMP rune (CJK ideograph) yields ONE '-', a non-BMP rune (emoji) TWO. Verified
// against CLI 2.1.219 ("/tmp/slugtest2/中文目录" → "-tmp-slugtest2-----").
// Invalid UTF-8 bytes decode as RuneError size 1 and contribute one '-' each.
//
// Exported because reverse-resolving a slug back to a real path walks the
// filesystem encoding one directory NAME at a time to compare against the
// remainder (discovery/recent.go); that comparison has to use this exact
// encoding or the walk silently fails to match.
func EncodeSegment(s string) string {
	needs := false
	for i := 0; i < len(s); i++ {
		if !isASCIIAlnum(s[i]) {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	// strings.Builder hands out its buffer without copying, keeping this at
	// one allocation per call on the sidebar-fetch / cron URL hot paths.
	var b strings.Builder
	// The output is never longer than the input: every ASCII-alnum byte maps
	// to itself, and any multi-byte rune shrinks to at most two dashes.
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if isASCIIAlnum(c) {
				b.WriteByte(c)
			} else {
				b.WriteByte('-')
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r > 0xFFFF {
			b.WriteString("--")
		} else {
			b.WriteByte('-')
		}
		i += size
	}
	return b.String()
}

func isASCIIAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// slugHash reproduces the CLI's overflow suffix, a Java-style 32-bit
// string hash of the original path rendered base36:
//
//	let t = 0; for (c of s) t = (t << 5) - t + c.charCodeAt(i) | 0
//	Math.abs(t).toString(36)
//
// int32 arithmetic matches JS's `| 0` wraparound; non-BMP runes are folded
// into surrogate pairs; Math.abs(-2^31) is 2^31 in JS, so that input is special-cased.
func slugHash(s string) string {
	var h int32
	for _, r := range s {
		if r > 0xFFFF {
			r -= 0x10000
			hi := int32(0xD800 + (r >> 10))
			lo := int32(0xDC00 + (r & 0x3FF))
			h = (h << 5) - h + hi
			h = (h << 5) - h + lo
			continue
		}
		h = (h << 5) - h + int32(r)
	}
	if h == math.MinInt32 {
		// JS: Math.abs(-2147483648) === 2147483648.
		return strconv.FormatUint(1<<31, 36)
	}
	if h < 0 {
		h = -h
	}
	return strconv.FormatInt(int64(h), 36)
}

// hasControlByte reports whether s contains any byte < 0x20 (no allocation).
func hasControlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 {
			return true
		}
	}
	return false
}

// stripControlBytes returns s with every byte < 0x20 removed; hasControlByte
// gates it so the typical cwd never pays the copy.
func stripControlBytes(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x20 {
			b = append(b, s[i])
		}
	}
	return string(b)
}

// IsValidSessionID reports whether s is a canonical lowercase-hex UUID, the
// shape the CLI uses for a transcript filename. Callers validate BEFORE building
// a path from it: the ID reaches naozhi from persisted state and HTTP requests,
// and it becomes a filename.
func IsValidSessionID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
