//go:build !linux

package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrUnsupportedPlatform is returned by every discovery entrypoint on
// non-Linux builds. Mirrors shim.ErrUnsupportedPlatform so cross-platform
// CI build jobs succeed while production (Linux-only) behavior is
// preserved.
var ErrUnsupportedPlatform = errors.New("shim/discovery not supported on this platform")

// MaxSafeJSONInt mirrors the constant in scanner.go so tests and callers
// that reference it by package-qualified name compile on every GOOS.
const MaxSafeJSONInt uint64 = (1 << 53) - 1

// pathCacheNegativeTTL / pathCacheMaxEntries / pathCacheEvictBatch are
// referenced by history.go. Values don't matter on non-Linux — the
// methods are no-ops — but the identifiers must exist.
const (
	pathCacheNegativeTTL = 60 * time.Second
	pathCacheMaxEntries  = 2048
	pathCacheEvictBatch  = 16
)

// DiscoveredSession mirrors the Linux-build struct so server-side
// literal construction (e.g. []DiscoveredSession{}) compiles.
type DiscoveredSession struct {
	PID           int    `json:"pid"`
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	StartedAt     int64  `json:"started_at"`
	LastActive    int64  `json:"last_active"`
	State         string `json:"state"`
	Kind          string `json:"kind"`
	Entrypoint    string `json:"entrypoint"`
	CLIName       string `json:"cli_name,omitempty"`
	Summary       string `json:"summary,omitempty"`
	LastPrompt    string `json:"last_prompt,omitempty"`
	ProcStartTime uint64 `json:"proc_start_time"`
	Project       string `json:"project,omitempty"`
	Node          string `json:"node,omitempty"`
}

// Scanner stub retains the pathCache field because history.go accesses
// it directly (s.pathCache.RLock/Lock). promptCache / summaryCache
// aren't accessed outside scanner.go, so they're omitted.
type Scanner struct {
	pathCache pathCacheState
}

type pathCacheState struct {
	sync.RWMutex
	entries map[string]pathCacheEntry
}

type pathCacheEntry struct {
	path          string
	negativeUntil time.Time
}

// sessionsIndex / sessionsIndexEntry are referenced by recent.go.
type sessionsIndex struct {
	OriginalPath string               `json:"originalPath"`
	Entries      []sessionsIndexEntry `json:"entries"`
}

type sessionsIndexEntry struct {
	SessionID   string `json:"sessionId"`
	Summary     string `json:"summary"`
	FirstPrompt string `json:"firstPrompt"`
}

func NewScanner() *Scanner {
	return &Scanner{pathCache: pathCacheState{entries: make(map[string]pathCacheEntry)}}
}

var (
	defaultScannerOnce sync.Once
	defaultScannerInst *Scanner
)

func DefaultScanner() *Scanner {
	defaultScannerOnce.Do(func() { defaultScannerInst = NewScanner() })
	return defaultScannerInst
}

func Scan(_ string, _ map[int]bool, _ map[string]bool, _ map[string]bool) ([]DiscoveredSession, error) {
	return nil, ErrUnsupportedPlatform
}

func (s *Scanner) Scan(_ string, _ map[int]bool, _ map[string]bool, _ map[string]bool) ([]DiscoveredSession, error) {
	return nil, ErrUnsupportedPlatform
}

func LookupSummaries(_ string, _ map[string]string) map[string]string { return nil }

func (s *Scanner) LookupSummaries(_ string, _ map[string]string) map[string]string { return nil }

func RefreshDynamic(_ string, _ []DiscoveredSession) bool { return false }

func (s *Scanner) RefreshDynamic(_ string, _ []DiscoveredSession) bool { return false }

// SanitizePromptForTransport is referenced by recent.go; keep the
// original behavior (pass-through for ASCII-clean input, underscore
// substitution for control bytes) so non-Linux builds do not silently
// diverge in the rare case someone exercises the helper through a
// test binary.
func SanitizePromptForTransport(s string) string {
	if s == "" {
		return s
	}
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, s)
}

// IsClaudeSystemInjectedText is referenced by history.go and recent.go.
// Kept as a real implementation so parse paths used in tests remain
// correct on every GOOS; this function is pure and has no syscalls.
func IsClaudeSystemInjectedText(text string) bool {
	if text == "[Request interrupted by user]" ||
		text == "[Request interrupted by user for tool use]" {
		return true
	}
	if len(text) < 3 || text[0] != '<' {
		return false
	}
	for _, name := range [...]string{
		"task-notification", "system-reminder", "local-command",
		"command-name", "available-deferred-tools",
	} {
		if len(text) < len(name)+2 {
			continue
		}
		if text[1:1+len(name)] != name {
			continue
		}
		next := text[1+len(name)]
		if next == '>' || next == ' ' || next == '\t' || next == '\n' || next == '\r' {
			return true
		}
	}
	return false
}

// ClaudeProjectSlug / projDirName are pure string transforms and are
// referenced by history.go, history_tail.go, recent.go, and by
// internal/session/router.go. Kept real on non-Linux.
func ClaudeProjectSlug(cwd string) string { return strings.ReplaceAll(cwd, "/", "-") }

func projDirName(cwd string) string { return ClaudeProjectSlug(cwd) }

// IsValidSessionID is called by recent.go, history_tail.go and by
// several non-discovery packages; kept as a real implementation so
// validation behavior matches across platforms.
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

// WaitAndCleanup is a no-op on non-Linux; production only ships on Linux.
func WaitAndCleanup(_ context.Context, _ int, _ uint64, _, _, _ string) {}

// extractUserText is a simplified stub; recent.go calls it but on
// non-Linux builds the containing RecentSessions flow is not exercised
// in production. Returning "" keeps callers well-defined.
func extractUserText(_ json.RawMessage) string { return "" }
