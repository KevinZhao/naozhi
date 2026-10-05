package clievent

import (
	"cmp"
	"net/url"
	"strings"
)

// CodeChange is a pull/merge request the session published or contributed to,
// from claude's system/code_change_published frame. The CLI scrapes the fields
// from command output, so they are a display hint: never route an
// authenticated request by them.
type CodeChange struct {
	Provider   string `json:"provider,omitempty"`
	URL        string `json:"url"`
	Repo       string `json:"repo,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Action     string `json:"action,omitempty"`
	Branch     string `json:"branch,omitempty"`
}

// MaxCodeChanges bounds the per-session list (most recent kept).
const MaxCodeChanges = 10

// Field caps for Valid, with headroom over the shapes the CLI emits.
const (
	maxCodeChangeURLBytes    = 2048
	maxCodeChangeRepoBytes   = 256
	maxCodeChangeTokenBytes  = 64
	maxCodeChangeBranchBytes = 255
)

// Valid reports whether c is safe to persist and render as a link: an http(s)
// URL with a host and no credentials, short single-line fields, and
// token-shaped provider/action/identifier. The frame comes off the CLI's
// stdout and sessions.json is hand-editable, so both entry points check it.
func (c CodeChange) Valid() bool {
	if len(c.URL) > maxCodeChangeURLBytes || hasControl(c.URL) {
		return false
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return false
	}
	return len(c.Repo) <= maxCodeChangeRepoBytes && !hasControl(c.Repo) &&
		len(c.Branch) <= maxCodeChangeBranchBytes && !hasControl(c.Branch) &&
		isCodeChangeToken(c.Identifier) && isCodeChangeToken(c.Provider) && isCodeChangeToken(c.Action)
}

// MergeCodeChange returns list with c recorded as the newest entry: an entry
// for the same URL moves to the end (keeping fields c omits), and the oldest
// entries drop past MaxCodeChanges. changed is false when c repeats the newest
// entry, which the CLI does on every push to a PR branch. list is not mutated.
func MergeCodeChange(list []CodeChange, c CodeChange) (out []CodeChange, changed bool) {
	out = make([]CodeChange, 0, min(len(list)+1, MaxCodeChanges))
	for _, old := range list {
		if old.URL != c.URL {
			out = append(out, old)
			continue
		}
		c.Provider = cmp.Or(c.Provider, old.Provider)
		c.Repo = cmp.Or(c.Repo, old.Repo)
		c.Identifier = cmp.Or(c.Identifier, old.Identifier)
		c.Action = cmp.Or(c.Action, old.Action)
		c.Branch = cmp.Or(c.Branch, old.Branch)
	}
	if n := len(list); n > 0 && list[n-1] == c {
		return list, false
	}
	out = append(out, c)
	if len(out) > MaxCodeChanges {
		out = out[len(out)-MaxCodeChanges:]
	}
	return out, true
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// isCodeChangeToken accepts "" or a short [A-Za-z0-9._-] token (provider
// names, gh pr verbs, change numbers).
func isCodeChangeToken(s string) bool {
	if len(s) > maxCodeChangeTokenBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_' || b == '.') {
			return false
		}
	}
	return true
}
