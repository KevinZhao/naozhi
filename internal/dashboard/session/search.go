package session

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Reply search bounds. Only the replies of each session's last searchTurns
// turns are searched, read from at most searchTailEntries entries of its tail,
// so one query never walks a whole transcript.
const (
	searchTurns       = 3
	searchTailEntries = 200
	// searchSessionBytes caps the reply text one history session keeps in
	// the search cache, newest first.
	searchSessionBytes = 32 << 10
	searchMaxHits      = 30
	searchMinRunes     = 2
	searchMaxRunes     = 100
	// searchReadBudget bounds the transcript reads of one query; sessions
	// it leaves unread are searched by the next one (the cache keeps what
	// was read) and the answer says partial.
	searchReadBudget = 2 * time.Second
	snippetRunes     = 80
)

// searchHit is one session whose recent replies hold the query: Key for a
// sidebar session, SessionID for a history one. Time and UUID name the reply
// event, so the dashboard can scroll to it (data-time / data-uuid).
type searchHit struct {
	Key       string `json:"key,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Time      int64  `json:"time"`
	UUID      string `json:"uuid,omitempty"`
	Snippet   string `json:"snippet"`
}

// searchResp is the GET /api/sessions/history?q= body. Hits is never null,
// newest reply first.
type searchResp struct {
	Hits    []searchHit `json:"hits"`
	Partial bool        `json:"partial,omitempty"`
}

// replyText is one assistant text event kept for search.
type replyText struct {
	time int64
	uuid string
	text string
}

// replyCacheEntry is a history session's recent replies as of the
// transcript mtime they were read at.
type replyCacheEntry struct {
	lastActive int64
	replies    []replyText
}

// replyCache holds the history sessions' recent replies between queries,
// pruned to the current history list on every query.
type replyCache struct {
	mu sync.Mutex
	m  map[string]replyCacheEntry

	// reading is held by the one query reading transcripts. A query arriving
	// meanwhile (the next keystroke, another tab) waits for it within its own
	// budget and then finds the cache filled, rather than reading the same
	// tails again.
	readingOnce sync.Once
	reading     chan struct{}
}

// readSlot is the reading semaphore, made on first use so a zero Handlers
// works.
func (c *replyCache) readSlot() chan struct{} {
	c.readingOnce.Do(func() { c.reading = make(chan struct{}, 1) })
	return c.reading
}

// loadHistoryTail is the transcript tail read; tests count the reads.
var loadHistoryTail = discovery.LoadHistoryTailCtx

// handleSearch answers GET /api/sessions/history?q=: the sidebar sessions
// (in-memory event log) and the history sessions (transcript tail) whose last
// turns' replies contain q, case-insensitively. A query shorter than
// searchMinRunes finds nothing; one longer than searchMaxRunes is cut.
func (h *Handlers) handleSearch(w http.ResponseWriter, r *http.Request, q string) {
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) > searchMaxRunes {
		q = string([]rune(q)[:searchMaxRunes])
	}
	resp := searchResp{Hits: []searchHit{}}
	if utf8.RuneCountInString(q) < searchMinRunes {
		httputil.WriteJSON(w, resp)
		return
	}
	needle := foldRunes(q)
	resp.Hits = append(resp.Hits, h.searchLive(needle)...)
	ctx, cancel := context.WithTimeout(r.Context(), searchReadBudget)
	defer cancel()
	hist, partial := h.searchHistory(ctx, needle)
	resp.Hits = append(resp.Hits, hist...)
	resp.Partial = partial
	slices.SortStableFunc(resp.Hits, func(a, b searchHit) int { return cmp.Compare(b.Time, a.Time) })
	if len(resp.Hits) > searchMaxHits {
		resp.Hits = resp.Hits[:searchMaxHits]
	}
	httputil.WriteJSON(w, resp)
}

// searchLive searches the sessions the sidebar lists.
func (h *Handlers) searchLive(needle []rune) []searchHit {
	snaps, _ := h.deps.Router.ListSessionsWithVersion()
	var hits []searchHit
	for _, snap := range snaps {
		if hiddenFromList(snap.Key) {
			continue
		}
		sess := h.deps.Router.SessionFor(snap.Key)
		if sess == nil {
			continue
		}
		if hit, ok := matchReplies(lastTurnReplies(sess.EventLastN(searchTailEntries), searchTurns, 0), needle); ok {
			hit.Key = snap.Key
			hits = append(hits, hit)
		}
	}
	return hits
}

// searchHistory searches the history list's sessions, reading the transcript
// tail of each one the cache does not hold at its current mtime. partial is
// true when ctx ended before every session was read. One query reads at a
// time; one whose ctx ends while it waits searches the cache alone.
func (h *Handlers) searchHistory(ctx context.Context, needle []rune) (hits []searchHit, partial bool) {
	list := h.historySessions()
	if len(list) == 0 || h.deps.ClaudeDir == "" {
		return nil, false
	}
	c := &h.replies
	slot := c.readSlot()
	reader := false
	select {
	case slot <- struct{}{}:
		reader = true
		defer func() { <-slot }()
	case <-ctx.Done():
	}
	c.mu.Lock()
	cached := c.m
	c.mu.Unlock()
	next := make(map[string]replyCacheEntry, len(list))
	for _, rs := range list {
		e, ok := cached[rs.SessionID]
		if !ok || e.lastActive != rs.LastActive {
			if !reader || ctx.Err() != nil {
				partial = true
				continue
			}
			entries, err := loadHistoryTail(ctx, h.deps.ClaudeDir, rs.SessionID, rs.Workspace, searchTailEntries)
			if err != nil || ctx.Err() != nil {
				// A read cut short is not cached: the next query retries it.
				partial = partial || ctx.Err() != nil
				continue
			}
			e = replyCacheEntry{lastActive: rs.LastActive, replies: lastTurnReplies(entries, searchTurns, searchSessionBytes)}
		}
		next[rs.SessionID] = e
		if hit, ok := matchReplies(e.replies, needle); ok {
			hit.SessionID = rs.SessionID
			hits = append(hits, hit)
		}
	}
	if reader {
		c.mu.Lock()
		c.m = next
		c.mu.Unlock()
	}
	return hits, partial
}

// lastTurnReplies returns the assistant text events of the last `turns`
// turns of entries (chronological), newest first. A turn starts at a user
// entry; text before the oldest counted one is not taken. maxBytes > 0 stops
// once that much text is kept.
func lastTurnReplies(entries []clievent.EventEntry, turns, maxBytes int) []replyText {
	var out []replyText
	size := 0
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch e.Type {
		case clievent.KindUser:
			if turns--; turns <= 0 {
				return out
			}
		case clievent.KindText:
			text := e.Detail
			if text == "" {
				text = e.Summary
			}
			if text == "" {
				continue
			}
			out = append(out, replyText{time: e.Time, uuid: e.UUID, text: text})
			if size += len(text); maxBytes > 0 && size >= maxBytes {
				return out
			}
		}
	}
	return out
}

// matchReplies finds needle (folded by foldRunes) in replies, newest first,
// and returns the first match's event with the sentence holding it.
func matchReplies(replies []replyText, needle []rune) (searchHit, bool) {
	for _, rp := range replies {
		rs := []rune(rp.text)
		if i := foldIndex(rs, needle); i >= 0 {
			return searchHit{Time: rp.time, UUID: rp.uuid, Snippet: snippetAround(rs, i, len(needle))}, true
		}
	}
	return searchHit{}, false
}

// foldRunes lowercases rune by rune, so an index into the result is an index
// into the input (strings.ToLower may change the length).
func foldRunes(s string) []rune {
	rs := []rune(s)
	for i, r := range rs {
		rs[i] = unicode.ToLower(r)
	}
	return rs
}

// foldIndex is the rune index of the first case-insensitive occurrence of
// needle (already folded) in rs, -1 when there is none.
func foldIndex(rs, needle []rune) int {
	if len(needle) == 0 {
		return -1
	}
	for i := 0; i+len(needle) <= len(rs); i++ {
		j := 0
		for j < len(needle) && unicode.ToLower(rs[i+j]) == needle[j] {
			j++
		}
		if j == len(needle) {
			return i
		}
	}
	return -1
}

// snippetAround is the sentence of rs holding the match at rune index i
// (length n), markdown stripped, cut to about snippetRunes around the match.
// A sentence ends at 。！？!?；, a newline, or ". " — not at a bare '.', which
// names like "g6e.12xlarge" hold.
func snippetAround(rs []rune, i, n int) string {
	start := i
	for start > 0 && !sentenceEnd(rs, start-1) {
		start--
	}
	end := i + n
	for end < len(rs) && !sentenceEnd(rs, end) {
		end++
	}
	if end < len(rs) && rs[end] != '\n' {
		end++ // keep the sentence's own punctuation
	}
	// Window the raw sentence first, so stripping a long one stays cheap.
	lead, trail := false, false
	if end-start > snippetRunes {
		from := max(start, i-(snippetRunes-n)/2)
		to := min(end, from+max(snippetRunes, n))
		from = max(start, min(from, to-snippetRunes))
		lead, trail = from > start, to < end
		start, end = from, to
	}
	// StripMarkdown keeps "**" whose closer touches a CJK rune ("**结论**是");
	// a bold marker is never the text searched for.
	s := strings.ReplaceAll(textutil.StripMarkdown(string(rs[start:end])), "**", "")
	if lead {
		s = "…" + s
	}
	if trail {
		s += "…"
	}
	return s
}

func sentenceEnd(rs []rune, k int) bool {
	switch rs[k] {
	case '。', '！', '？', '；', '!', '?', '\n':
		return true
	case '.':
		return k+1 < len(rs) && (rs[k+1] == ' ' || rs[k+1] == '\n')
	}
	return false
}
