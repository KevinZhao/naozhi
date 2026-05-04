package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli"
)

// ScratchKeyPrefix is the session-key prefix used for all ephemeral "aside"
// sessions created via the scratch pool. Session-list, persistence, and
// sidebar paths filter on this prefix so scratch sessions never surface in
// the normal sidebar or get written to sessions.json. Mutations to this
// constant must be accompanied by updates in saveStore (filter) and the
// dashboard handleList filter.
//
// R176-ARCH-M1: canonical reserved-namespace prefixes are listed together in
// key.go (reservedKeyPrefixes); this constant lives here for historical
// proximity to the ScratchPool implementation.
const ScratchKeyPrefix = "scratch:"

// MaxScratchQuoteBytes caps the quoted context passed to --append-system-prompt.
// 8 KiB covers several paragraphs of ordinary text while keeping the spawn
// arg list from bloating NDJSON frames on ACP protocols that mirror CLI args
// into control messages.
const MaxScratchQuoteBytes = 8 * 1024

// MaxScratchContextBytes caps the total rendered size of the conversation-context
// block (user/assistant turns surrounding the quote) + the quote itself inside
// the --append-system-prompt arg. 24 KiB stays well under POSIX ARG_MAX while
// fitting ~5 turns of ordinary English plus a full 8 KiB quote.
const MaxScratchContextBytes = 24 * 1024

// DefaultScratchContextTurns is the default number of user/assistant turns to
// pull from the source session on either side of the quoted message. A "turn"
// here counts a single user or assistant entry (not a matched pair), so 5
// yields roughly 2-3 exchanges before + 2-3 after.
const DefaultScratchContextTurns = 5

// MaxScratchContextTurns caps the client-requested turn count so a malicious
// or buggy client cannot force the server to serialize hundreds of entries
// just to have them thrown away by the byte budget.
const MaxScratchContextTurns = 20

// DefaultScratchTTL is how long an idle scratch session can live before the
// pool sweeper kills it. Shorter than Router.DefaultTTL because scratches are
// meant to be used and discarded — a forgotten tab shouldn't tie up a CLI
// process slot for half an hour.
const DefaultScratchTTL = 10 * time.Minute

// DefaultScratchMax is the global concurrent-scratch cap. Each scratch owns
// a real CLI process, so the pool shares Router.MaxProcs headroom — pick a
// value that leaves room for main sessions. 20 mirrors maxExemptSessions.
const DefaultScratchMax = 20

// Errors surfaced by ScratchPool callers. Kept as sentinels so HTTP handlers
// can translate them into 4xx / 429 responses without string matching.
var (
	ErrScratchPoolFull = errors.New("scratch pool full")
	ErrScratchNotFound = errors.New("scratch not found")
	ErrQuoteEmpty      = errors.New("quote is empty after sanitization")
)

// Scratch is a single ephemeral aside session.
//
// It inherits the source session's AgentOpts (backend, model, extra args,
// workspace) and prepends a `--append-system-prompt` carrying the quoted
// context so the CLI answers with knowledge of what the user selected,
// without the quote ever being echoed in the visible transcript.
//
// The router owns the process lifecycle once the scratch is registered;
// ScratchPool keeps only metadata and a router key it can use to tear the
// session down on Close or TTL expiry.
type Scratch struct {
	ID           string    // 16-byte hex (32 chars)
	Key          string    // full router key: "scratch:<id>:general:<sourceAgentID>"
	SourceKey    string    // key of the session the user quoted from
	AgentID      string    // inherited from source
	Backend      string    // inherited from source (empty = router default)
	Workspace    string    // inherited from source
	Quote        string    // sanitized, truncated quote
	QuoteTrunc   bool      // true when the quote was truncated at MaxScratchQuoteBytes
	ContextTurns int       // number of surrounding turns actually rendered into the system prompt
	ContextTrunc bool      // true when the context block was shrunk to fit the byte budget
	BaseOpts     AgentOpts // full opts the router will receive on first spawn
	CreatedAt    time.Time

	lastUsed atomic.Int64 // unix nano; touched on every send
}

// LastUsed returns the last activity timestamp. Lock-free.
func (s *Scratch) LastUsed() time.Time {
	return time.Unix(0, s.lastUsed.Load())
}

// Touch updates the last-used timestamp. Called on every send so the sweeper
// treats an actively used scratch as fresh.
func (s *Scratch) Touch() {
	s.lastUsed.Store(time.Now().UnixNano())
}

// ScratchPool manages the set of live ephemeral scratch sessions.
//
// The pool does NOT spawn processes itself — it registers opts so that when
// the first dashboard send hits the router with a `scratch:` key, the router's
// GetOrCreate path spawns a real CLI exactly like any other session. This
// keeps the spawn/send/event paths identical to managed sessions (same SSE
// streaming, same state broadcasts, same InjectHistory semantics) and avoids
// a parallel protocol stack.
//
// On Close / TTL expiry the pool calls router.Remove(key) which kills the
// process and drops the session entry — scratches never persist through a
// restart.
type ScratchPool struct {
	mu       sync.Mutex
	items    map[string]*Scratch // ID -> Scratch
	byKey    map[string]*Scratch // router key -> Scratch (for BaseOpts lookup on spawn)
	max      int
	ttl      time.Duration
	router   *Router
	stopOnce sync.Once
	stopCh   chan struct{}
	sweepWG  sync.WaitGroup
}

// NewScratchPool constructs a pool bound to router. max and ttl are clamped
// to sensible defaults when non-positive.
func NewScratchPool(router *Router, max int, ttl time.Duration) *ScratchPool {
	if max <= 0 {
		max = DefaultScratchMax
	}
	if ttl <= 0 {
		ttl = DefaultScratchTTL
	}
	return &ScratchPool{
		items:  make(map[string]*Scratch),
		byKey:  make(map[string]*Scratch),
		max:    max,
		ttl:    ttl,
		router: router,
		stopCh: make(chan struct{}),
	}
}

// StartSweeper launches the background TTL goroutine. Safe to call once.
// The sweeper runs until Stop() is invoked; sweep cadence is ttl/2.
func (p *ScratchPool) StartSweeper() {
	p.sweepWG.Add(1)
	go func() {
		defer p.sweepWG.Done()
		tick := p.ttl / 2
		if tick < 30*time.Second {
			tick = 30 * time.Second
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case now := <-t.C:
				p.sweep(now)
			}
		}
	}()
}

// Stop signals the sweeper to exit and waits for it. Idempotent.
func (p *ScratchPool) Stop() {
	p.stopOnce.Do(func() { close(p.stopCh) })
	p.sweepWG.Wait()
}

// sweep removes scratches idle past TTL. Router.Remove() is called outside
// the pool lock to avoid holding our mutex during potentially slow process
// teardown.
func (p *ScratchPool) sweep(now time.Time) {
	cutoff := now.Add(-p.ttl).UnixNano()
	var expired []*Scratch
	p.mu.Lock()
	for id, sc := range p.items {
		if sc.lastUsed.Load() < cutoff {
			expired = append(expired, sc)
			delete(p.items, id)
			delete(p.byKey, sc.Key)
		}
	}
	p.mu.Unlock()
	for _, sc := range expired {
		if p.router != nil {
			p.router.Remove(sc.Key)
		}
	}
}

// OpenOptions configures a new scratch session.
type OpenOptions struct {
	SourceKey string    // required: key of the session being quoted from
	AgentID   string    // required: source session's agent ID
	Backend   string    // source session's backend (empty = router default)
	Workspace string    // source session's workspace
	BaseOpts  AgentOpts // router-resolved AgentOpts for the source agent (model / extra args / workspace)
	Quote     string    // the text the user selected
	// ContextBefore/ContextAfter are event entries surrounding the quoted
	// message in the source session's event log, in chronological order.
	// Callers should pre-filter to user/text/result types only — Open
	// defensively re-filters for safety but trusts the caller's ordering.
	// These feed the <conversation_context> block in the system prompt so
	// the CLI sees a few turns of surrounding conversation rather than a
	// lone quoted snippet. Either slice may be empty.
	ContextBefore []cli.EventEntry
	ContextAfter  []cli.EventEntry
}

// Open creates a new scratch session and returns it. The quote is sanitized
// and truncated; the resulting --append-system-prompt is appended to
// opts.BaseOpts.ExtraArgs. The caller is responsible for first validating
// that opts.SourceKey refers to a real session.
func (p *ScratchPool) Open(opts OpenOptions) (*Scratch, error) {
	clean, truncated := SanitizeQuote(opts.Quote)
	if clean == "" {
		return nil, ErrQuoteEmpty
	}

	id, err := newScratchID()
	if err != nil {
		return nil, fmt.Errorf("scratch id: %w", err)
	}
	agentID := opts.AgentID
	if agentID == "" {
		agentID = "general"
	}
	// Key shape: scratch:<id>:general:<sourceAgent>
	//
	// Session key is still 4 segments so ValidateSessionKey and every
	// {platform}:{chatType}:{id}:{agentID} parser that splits by ':'
	// keeps working. "scratch" holds the platform slot, the scratch ID
	// holds the chat-type slot (dedup-safe), "general" holds the chat-ID
	// slot (fixed filler so chat-key extraction yields "scratch:<id>:general"),
	// and the agent slot records the source agent for telemetry + promote.
	key := ScratchKeyPrefix + id + ":general:" + sanitizeKeyComponent(agentID)

	// Render the surrounding-turn context block under a shared byte budget
	// with the quote: quote takes priority (bounded at MaxScratchQuoteBytes),
	// context gets whatever is left of MaxScratchContextBytes. The renderer
	// drops noisy event types (tool_use / thinking / init / system / todo)
	// and fills from the turns closest to the quote outward so truncation
	// lops off the most distant history first.
	contextBudget := MaxScratchContextBytes - len(clean)
	if contextBudget < 0 {
		contextBudget = 0
	}
	contextBlock, ctxTurns, ctxTrunc := renderContextTurns(
		opts.ContextBefore, opts.ContextAfter, contextBudget,
	)

	// Build BaseOpts: deep-copy the source opts so the scratch-specific
	// --append-system-prompt doesn't mutate the agent registry map value.
	cloned := opts.BaseOpts
	cloned.ExtraArgs = append([]string(nil), opts.BaseOpts.ExtraArgs...)
	cloned.ExtraArgs = append(cloned.ExtraArgs,
		"--append-system-prompt", buildScratchSystemPrompt(clean, truncated, contextBlock),
	)
	if opts.Workspace != "" {
		cloned.Workspace = opts.Workspace
	}
	if opts.Backend != "" {
		cloned.Backend = opts.Backend
	}
	// Scratches must never be exempt — we want them to count against maxProcs
	// and get evicted on TTL, not enter the planner-only code paths.
	cloned.Exempt = false

	sc := &Scratch{
		ID:           id,
		Key:          key,
		SourceKey:    opts.SourceKey,
		AgentID:      agentID,
		Backend:      opts.Backend,
		Workspace:    opts.Workspace,
		Quote:        clean,
		QuoteTrunc:   truncated,
		ContextTurns: ctxTurns,
		ContextTrunc: ctxTrunc,
		BaseOpts:     cloned,
		CreatedAt:    time.Now(),
	}
	sc.lastUsed.Store(sc.CreatedAt.UnixNano())

	p.mu.Lock()
	if len(p.items) >= p.max {
		p.mu.Unlock()
		return nil, ErrScratchPoolFull
	}
	p.items[id] = sc
	p.byKey[key] = sc
	p.mu.Unlock()
	return sc, nil
}

// Get returns the scratch for a given ID or nil.
func (p *ScratchPool) Get(id string) *Scratch {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.items[id]
}

// OptsForKey returns the registered BaseOpts for a router key, or zero-value
// + false when the key is not a scratch managed by this pool. Called from
// hub.sessionOptsFor so the router receives the inherited agent configuration
// on first spawn.
//
// SWEEP-DEFENSE INVARIANT: this method calls sc.Touch() on every hit so the
// very first /api/sessions/send for a freshly-opened scratch cannot lose a
// race with the TTL sweeper. If a future refactor removes the Touch here,
// introduce an equivalent guard BEFORE dropping it — otherwise a scratch
// whose open-to-first-send latency exceeds the sweep interval (e.g. the
// user took a bathroom break) will silently 404 on send.
func (p *ScratchPool) OptsForKey(key string) (AgentOpts, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sc, ok := p.byKey[key]
	if !ok {
		return AgentOpts{}, false
	}
	sc.Touch()
	return sc.BaseOpts, true
}

// Touch updates the last-used timestamp for a scratch keyed by router key.
// Called on every send so the sweeper treats active scratches as fresh.
func (p *ScratchPool) Touch(key string) {
	p.mu.Lock()
	sc, ok := p.byKey[key]
	p.mu.Unlock()
	if ok {
		sc.Touch()
	}
}

// Close removes the scratch by ID and tears down its router-side session.
// Returns ErrScratchNotFound when the ID is unknown.
func (p *ScratchPool) Close(id string) error {
	p.mu.Lock()
	sc, ok := p.items[id]
	if !ok {
		p.mu.Unlock()
		return ErrScratchNotFound
	}
	delete(p.items, id)
	delete(p.byKey, sc.Key)
	p.mu.Unlock()
	if p.router != nil {
		p.router.Remove(sc.Key)
	}
	return nil
}

// Detach removes the scratch metadata WITHOUT killing the router session.
// Used by Promote: the caller is about to repurpose the live CLI process
// under a new session key, so the pool relinquishes ownership without
// tearing the process down. Returns the scratch for inspection.
func (p *ScratchPool) Detach(id string) (*Scratch, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sc, ok := p.items[id]
	if !ok {
		return nil, ErrScratchNotFound
	}
	delete(p.items, id)
	delete(p.byKey, sc.Key)
	return sc, nil
}

// List returns a snapshot of all live scratches. Used by tests and debug
// endpoints; production callers should prefer Get / OptsForKey.
func (p *ScratchPool) List() []*Scratch {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Scratch, 0, len(p.items))
	for _, sc := range p.items {
		out = append(out, sc)
	}
	return out
}

// Len returns the current number of live scratches.
func (p *ScratchPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

// ForceExpireForTest backdates a scratch's lastUsed timestamp so the next
// sweep evicts it. Test-only seam — production callers should never move
// timestamps backwards.
func (p *ScratchPool) ForceExpireForTest(id string, t time.Time) {
	p.mu.Lock()
	sc, ok := p.items[id]
	p.mu.Unlock()
	if ok {
		sc.lastUsed.Store(t.UnixNano())
	}
}

// SweepForTest runs one eviction pass synchronously. Exported so tests can
// exercise the cleanup path without waiting on the ticker goroutine.
func (p *ScratchPool) SweepForTest(now time.Time) { p.sweep(now) }

// IsScratchKey reports whether a session key belongs to the scratch pool.
// Used by persistence (saveStore) and the dashboard sidebar filter to hide
// scratches from places they should not appear.
func IsScratchKey(key string) bool {
	return strings.HasPrefix(key, ScratchKeyPrefix)
}

// SanitizeQuote strips control characters and invisible Unicode formatting
// codepoints from s, truncating the result at MaxScratchQuoteBytes along a
// valid UTF-8 boundary. Newlines and horizontal tabs are preserved — quoted
// text often depends on line breaks for readability.
//
// Returns the cleaned string and a flag indicating whether the original
// exceeded MaxScratchQuoteBytes.
func SanitizeQuote(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	// Drop the C0 control set except \n (U+000A) and \t (U+0009), DEL, and
	// the C1 control range. Bidi / zero-width / BOM codepoints are removed
	// with the same rule sanitizeKeyComponent uses for log attrs — without
	// them a quoted shell prompt could rewrite operator journalctl output
	// via ANSI / bidi overrides.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		if r < 0x20 || (r >= 0x7F && r <= 0x9F) {
			continue
		}
		switch {
		case r >= 0x200B && r <= 0x200F,
			r >= 0x202A && r <= 0x202E,
			r == 0x2028, r == 0x2029,
			r == 0xFEFF:
			continue
		}
		b.WriteRune(r)
	}
	cleaned := b.String()

	truncated := false
	if len(cleaned) > MaxScratchQuoteBytes {
		truncated = true
		// Walk back to the previous valid rune boundary so the truncated
		// string stays valid UTF-8. The stdlib does this for us via DecodeLastRune
		// but the explicit loop avoids allocating a rune slice.
		cut := MaxScratchQuoteBytes
		for cut > 0 && !utf8.RuneStart(cleaned[cut]) {
			cut--
		}
		cleaned = cleaned[:cut]
	}
	// Final trim of trailing ASCII whitespace so truncation doesn't leave a
	// dangling half-line the CLI might interpret as meaningful framing.
	cleaned = strings.TrimRight(cleaned, " \t\n")
	return cleaned, truncated
}

// buildScratchSystemPrompt formats the quoted context for --append-system-prompt.
// The prompt explicitly instructs the model NOT to echo the quote back so the
// aside transcript stays focused on the answer.
//
// When contextBlock is non-empty it is inserted as a <conversation_context>
// section preceding the <selected_quote>, giving the model the surrounding
// turns the user was reading when they clicked "aside".
//
// PRIVACY TRADE-OFF: the returned string is handed to the child CLI as a
// distinct argv element. On Linux this is visible to any process on the same
// host that can read /proc/<pid>/cmdline — by default world-readable for the
// same UID via /proc, and surfaced by `ps`, systemd journal snapshots, and
// any shim state file that records argv. For naozhi's single-operator
// deployment model this is acceptable (no other tenants share the host),
// but any future multi-tenant deployment must route the quoted context
// through stdin or an env var instead of argv.
func buildScratchSystemPrompt(quote string, truncated bool, contextBlock string) string {
	var b strings.Builder
	b.WriteString("用户正在就主对话中选中的以下内容进行追问。请基于此内容回答后续问题，不要在回复中重复引用原文。")
	if contextBlock != "" {
		b.WriteString("\n\n<conversation_context>\n")
		b.WriteString(contextBlock)
		b.WriteString("\n</conversation_context>")
	}
	b.WriteString("\n\n<selected_quote>\n")
	b.WriteString(quote)
	if truncated {
		b.WriteString("\n…[已截断]")
	}
	b.WriteString("\n</selected_quote>")
	return b.String()
}

// renderContextTurns serialises a handful of user/assistant turns surrounding
// the quoted message into a plain-text block suitable for embedding in the
// system prompt.
//
// Event type filter: only `user`, `text`, and `result` entries contribute.
// Tool-use / thinking / init / system / todo / agent events are dropped
// because they bloat the budget with machine-oriented noise the model does
// not need to reconstruct the conversational gist.
//
// Budget policy: we fill from the turns closest to the quote outward (last
// entries of `before`, first entries of `after`) and stop as soon as adding
// another turn would exceed budgetBytes. The returned ctxTurns counts how
// many entries actually made it in; ctxTrunc reports whether any candidate
// entries were rejected. An empty block (no candidates survived filtering)
// returns ("", 0, false).
func renderContextTurns(before, after []cli.EventEntry, budgetBytes int) (string, int, bool) {
	// Filter once up front so both the zero-budget short-circuit and the
	// normal-budget walk share a single allocation. Previously the zero-
	// budget arm re-filtered just to check len() > 0, wasting two slices
	// on the hot path.
	beforeFiltered := filterContextEntries(before)
	afterFiltered := filterContextEntries(after)
	totalCandidates := len(beforeFiltered) + len(afterFiltered)

	if budgetBytes <= 0 {
		// No room even for a single byte. Signal truncated=true when we had
		// candidates so the UI / logs can mention context was suppressed.
		return "", 0, totalCandidates > 0
	}
	if totalCandidates == 0 {
		return "", 0, false
	}

	// Walk outward from the quote: before is consumed newest-first (tail),
	// after is consumed oldest-first (head). Rendered strings are cached
	// alongside a byte count so we can decide inclusion without rendering
	// twice.
	type rendered struct {
		text  string
		bytes int
	}
	beforeStack := make([]rendered, 0, len(beforeFiltered))
	for i := len(beforeFiltered) - 1; i >= 0; i-- {
		line := renderTurnLine(beforeFiltered[i])
		beforeStack = append(beforeStack, rendered{text: line, bytes: len(line)})
	}
	afterQueue := make([]rendered, 0, len(afterFiltered))
	for i := range afterFiltered {
		line := renderTurnLine(afterFiltered[i])
		afterQueue = append(afterQueue, rendered{text: line, bytes: len(line)})
	}

	// Reconstruct chronological order on the fly using two pointers. `used`
	// tracks the actual output length — we only charge a join-newline when
	// there is already content, so N entries cost sum(len(line)) + (N-1)
	// bytes rather than +N. Earlier code overcounted by 1 byte and could
	// reject an entry that actually fit, producing a spurious truncated=true.
	includedBefore := make([]string, 0, len(beforeStack))
	includedAfter := make([]string, 0, len(afterQueue))
	used := 0
	bi, ai := 0, 0
	// Alternate: prefer the side with more remaining candidates so extreme
	// imbalance (e.g. no `before` events) still fills the budget. Ties go to
	// `before` because the most recent prior turn is usually more relevant
	// than the next reply.
	for bi < len(beforeStack) || ai < len(afterQueue) {
		var pick *rendered
		var isBefore bool
		switch {
		case bi >= len(beforeStack):
			pick, isBefore = &afterQueue[ai], false
		case ai >= len(afterQueue):
			pick, isBefore = &beforeStack[bi], true
		default:
			if len(beforeStack)-bi >= len(afterQueue)-ai {
				pick, isBefore = &beforeStack[bi], true
			} else {
				pick, isBefore = &afterQueue[ai], false
			}
		}
		// Join-newline is only charged when there is already content in
		// either side's inclusion list — avoids the +1 overcount that
		// used to make `used` N bytes higher than actual output length.
		cost := pick.bytes
		if len(includedBefore)+len(includedAfter) > 0 {
			cost++ // newline between this entry and the previous one
		}
		if used+cost > budgetBytes {
			break
		}
		used += cost
		if isBefore {
			includedBefore = append(includedBefore, pick.text)
			bi++
		} else {
			includedAfter = append(includedAfter, pick.text)
			ai++
		}
	}

	if len(includedBefore) == 0 && len(includedAfter) == 0 {
		return "", 0, totalCandidates > 0
	}

	// includedBefore was collected tail-first (newest prior turn first). Flip
	// it back to chronological order before concatenation.
	for i, j := 0, len(includedBefore)-1; i < j; i, j = i+1, j-1 {
		includedBefore[i], includedBefore[j] = includedBefore[j], includedBefore[i]
	}

	var b strings.Builder
	b.Grow(used)
	for i, line := range includedBefore {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	if len(includedBefore) > 0 && len(includedAfter) > 0 {
		b.WriteByte('\n')
	}
	for i, line := range includedAfter {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}

	turns := len(includedBefore) + len(includedAfter)
	truncated := turns < totalCandidates
	return b.String(), turns, truncated
}

// filterContextEntries keeps only event types that carry conversational
// meaning (user prompts and assistant text / result replies). Everything
// else — tool_use, thinking, init, system, todo, agent, task_* — is dropped.
// The returned slice is a new allocation, not aliased.
func filterContextEntries(in []cli.EventEntry) []cli.EventEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]cli.EventEntry, 0, len(in))
	for _, e := range in {
		switch e.Type {
		case "user", "text", "result":
			// "result" and "text" can both appear in the same turn (text is
			// the streaming block, result is the final envelope); we keep
			// both because either may carry the visible reply depending on
			// when the source session was captured.
		default:
			continue
		}
		// Skip entries whose textual payload is empty after picking the
		// preferred field — rendering them would emit a naked role label
		// and waste budget.
		if pickEntryText(e) == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// renderTurnLine formats a single event entry as a role-tagged line suitable
// for the <conversation_context> block. The role comes from the entry type;
// the payload is sanitized (control chars / bidi stripped) and truncated so
// one noisy multi-KB entry cannot eat the whole budget on its own.
func renderTurnLine(e cli.EventEntry) string {
	role := "assistant"
	if e.Type == "user" {
		role = "user"
	}
	payload, _ := SanitizeQuote(pickEntryText(e)) // reuse control-char / bidi scrubber
	const perTurnCap = 2 * 1024                   // 2 KiB per rendered turn keeps any single entry from dominating
	if len(payload) > perTurnCap {
		cut := perTurnCap
		for cut > 0 && !utf8.RuneStart(payload[cut]) {
			cut--
		}
		payload = payload[:cut] + "…"
	}
	return "[" + role + "] " + payload
}

// pickEntryText returns the best textual payload for a context entry,
// preferring Detail (fuller form used by dashboard) and falling back to
// Summary when Detail is empty.
func pickEntryText(e cli.EventEntry) string {
	if e.Detail != "" {
		return e.Detail
	}
	return e.Summary
}

// newScratchID returns a 32-char lowercase hex string backed by crypto/rand.
// 128 bits of entropy is sufficient: the ID is ephemeral, scoped to a single
// pool, and lookups are keyed by the full string.
func newScratchID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
