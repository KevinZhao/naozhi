package turn

import (
	"strings"
	"unicode"
)

// CmdKind is the turn-level command Parse recognised.
type CmdKind uint8

const (
	CmdNone        CmdKind = iota // not a turn command; Text may still be another slash command
	CmdReset                      // "/new" or "/clear", with an optional Arg
	CmdUrgent                     // "/urgent <message>"; Arg is the message
	CmdUrgentUsage                // a bare "/urgent"
)

// Cmd is Parse's result. Text is the input trimmed and run through
// NormalizeCommand, for callers that go on to match other slash commands.
type Cmd struct {
	Kind CmdKind
	Arg  string
	Text string
}

// NormalizeCommand lowercases the leading "/command" token only (CJK IMEs
// auto-capitalize, e.g. "/New foo") and strips trailing whitespace so an IME
// that appends a space does not break a bare "/help" equality check. Text
// that does not start with "/" is returned unchanged.
func NormalizeCommand(trimmed string) string {
	if !strings.HasPrefix(trimmed, "/") {
		return trimmed
	}
	sp := strings.IndexByte(trimmed, ' ')
	if sp < 0 {
		// No ASCII space, but trailing unicode whitespace (U+3000) may remain.
		return strings.TrimRightFunc(strings.ToLower(trimmed), unicode.IsSpace)
	}
	return strings.TrimRightFunc(strings.ToLower(trimmed[:sp])+trimmed[sp:], unicode.IsSpace)
}

// Parse recognises the commands that act on turns: /new and /clear (reset),
// and /urgent. The command token is case-insensitive; the argument keeps its
// case. Everything else is CmdNone.
func Parse(text string) Cmd {
	t := NormalizeCommand(strings.TrimSpace(text))
	c := Cmd{Text: t}
	switch {
	case t == "/new" || t == "/clear":
		c.Kind = CmdReset
	case strings.HasPrefix(t, "/new ") || strings.HasPrefix(t, "/clear "):
		c.Kind = CmdReset
		c.Arg = strings.TrimSpace(t[strings.IndexByte(t, ' '):])
	case t == "/urgent":
		c.Kind = CmdUrgentUsage
	case strings.HasPrefix(t, "/urgent "):
		// t has no trailing whitespace, so the argument is never empty here.
		c.Kind = CmdUrgent
		c.Arg = strings.TrimSpace(strings.TrimPrefix(t, "/urgent "))
	}
	return c
}
