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

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// ScratchKeyPrefix re-exports sessionkey.ScratchKeyPrefix.
//
// Deprecated: use sessionkey.ScratchKeyPrefix.
const ScratchKeyPrefix = sessionkey.ScratchKeyPrefix

// MaxScratchQuoteBytes caps the quoted context passed to --append-system-prompt;
// 8 KiB keeps the spawn arg list from bloating NDJSON frames on ACP protocols
// that mirror CLI args into control messages.
const MaxScratchQuoteBytes = 8 * 1024

// MaxScratchContextBytes caps the rendered conversation-context block plus the
// quote inside the --append-system-prompt arg (well under POSIX ARG_MAX).
const MaxScratchContextBytes = 24 * 1024

// DefaultScratchContextTurns is the default number of user/assistant entries
// (not pairs) pulled from the source session on either side of the quote.
const DefaultScratchContextTurns = 5

// MaxScratchContextTurns caps the client-requested turn count so a client
// cannot force the server to serialize hundreds of entries the byte budget
// would discard anyway.
const MaxScratchContextTurns = 20

// DefaultScratchTTL is how long an idle scratch lives before the sweeper kills
// it. Shorter than Router.DefaultTTL: a forgotten tab must not hold a CLI slot.
const DefaultScratchTTL = 10 * time.Minute

// DefaultScratchMax is the global concurrent-scratch cap. Each scratch owns a
// real CLI process sharing Router.MaxProcs headroom; 20 mirrors maxExemptSessions.
const DefaultScratchMax = 20

// Errors surfaced by ScratchPool callers; sentinels so HTTP handlers can map
// them to 4xx / 429 without string matching.
var (
	ErrScratchPoolFull = errors.New("scratch pool full")
	ErrScratchNotFound = errors.New("scratch not found")
	ErrQuoteEmpty      = errors.New("quote is empty after sanitization")
)

// Scratch is a single ephemeral aside session. It inherits the source
// session's AgentOpts and carries the quoted context in the system prompt. The
// router owns the process lifecycle; the pool keeps only metadata and the
// router key it tears the session down with on Close or TTL expiry.
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

// Touch updates the last-used timestamp so the sweeper treats an actively
// used scratch as fresh.
func (s *Scratch) Touch() {
	s.lastUsed.Store(time.Now().UnixNano())
}

// ScratchPool manages the set of live ephemeral scratch sessions. It does NOT
// spawn processes: it registers opts so the router's GetOrCreate path spawns a
// real CLI on the first `scratch:` send. Close / TTL expiry call
// router.Remove(key); scratches never persist through a restart.
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
// to defaults when non-positive.
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

// StartSweeper launches the background TTL goroutine (cadence ttl/2, min 30s).
// Call once; runs until Stop().
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

// sweep removes scratches idle past TTL. Router.Remove() runs outside the pool
// lock so slow process teardown never holds p.mu. The O(N) walk is fine while
// the pool is capped at DefaultScratchMax.
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
	// ContextBefore/ContextAfter surround the quoted message in chronological
	// order; Open re-filters types but trusts the caller's ordering.
	ContextBefore []clievent.EventEntry
	ContextAfter  []clievent.EventEntry
}

// Open creates a new scratch session. The quote is sanitized and truncated;
// the resulting prompt is layered onto BaseOpts.SystemPrompt and reaches the
// CLI via cli.SpawnOptions.AppendSystemPrompt. The caller must first validate
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
	// Key stays 4 segments so every {platform}:{chatType}:{id}:{agentID} parser
	// keeps working; "general" is a fixed chat-ID filler and the agent slot
	// records the source agent for telemetry + promote.
	key := ScratchKeyPrefix + id + ":general:" + sanitizeKeyComponent(agentID)

	// Quote takes priority; the context block gets what is left of the budget.
	contextBudget := MaxScratchContextBytes - len(clean)
	if contextBudget < 0 {
		contextBudget = 0
	}
	contextBlock, ctxTurns, ctxTrunc := renderContextTurns(
		opts.ContextBefore, opts.ContextAfter, contextBudget,
	)

	// Copy the source opts (ExtraArgs deep-copied so a later append cannot
	// reach the agent registry's backing array) and layer the quoted context
	// onto the agent's own system prompt. The prompt must NOT go into ExtraArgs,
	// where cli.deniedExtraFlags strips --append-system-prompt (#2493).
	cloned := opts.BaseOpts
	cloned.ExtraArgs = append([]string(nil), opts.BaseOpts.ExtraArgs...)
	cloned.SystemPrompt = JoinSystemPrompts(
		opts.BaseOpts.SystemPrompt,
		buildScratchSystemPrompt(clean, truncated, contextBlock),
	)
	if opts.Workspace != "" {
		cloned.Workspace = opts.Workspace
	}
	if opts.Backend != "" {
		cloned.Backend = opts.Backend
	}
	// Scratches must never be exempt: they count against maxProcs and get
	// evicted on TTL rather than entering the planner-only code paths.
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
// + false when the key is not a scratch managed by this pool.
//
// SWEEP-DEFENSE INVARIANT: every hit calls sc.Touch() so the very first send
// for a freshly-opened scratch cannot lose a race with the TTL sweeper. Any
// refactor removing the Touch must add an equivalent guard first, or a scratch
// whose open-to-first-send latency exceeds the sweep interval 404s on send.
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
// Used by Promote, which repurposes the live CLI process under a new key.
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

// List returns a snapshot of all live scratches (tests / debug endpoints).
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
// sweep evicts it. Test-only seam.
func (p *ScratchPool) ForceExpireForTest(id string, t time.Time) {
	p.mu.Lock()
	sc, ok := p.items[id]
	p.mu.Unlock()
	if ok {
		sc.lastUsed.Store(t.UnixNano())
	}
}

// SweepForTest runs one eviction pass synchronously.
func (p *ScratchPool) SweepForTest(now time.Time) { p.sweep(now) }

// IsScratchKey reports whether a session key belongs to the scratch pool.
//
// Deprecated: use sessionkey.IsScratchKey.
func IsScratchKey(key string) bool { return sessionkey.IsScratchKey(key) }

// SanitizeQuote strips control characters and invisible Unicode formatting
// codepoints from s, truncating at MaxScratchQuoteBytes on a valid UTF-8
// boundary. Newlines and tabs are preserved. Returns the cleaned string and
// whether the original exceeded MaxScratchQuoteBytes.
func SanitizeQuote(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	// Deny-set is sessionkey.IsForbiddenKeyRune (C0/C1/DEL/bidi/zero-width/BOM,
	// #2301); only the \n / \t exemption is local. Without it a quoted shell
	// prompt could rewrite operator journalctl output via ANSI / bidi overrides.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		if sessionkey.IsForbiddenKeyRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := b.String()

	truncated := false
	if len(cleaned) > MaxScratchQuoteBytes {
		truncated = true
		// Walk back to a rune boundary so the result stays valid UTF-8.
		cut := MaxScratchQuoteBytes
		for cut > 0 && !utf8.RuneStart(cleaned[cut]) {
			cut--
		}
		cleaned = cleaned[:cut]
	}
	// Trim trailing whitespace so truncation cannot leave a dangling half-line.
	cleaned = strings.TrimRight(cleaned, " \t\n")
	return cleaned, truncated
}

// buildScratchSystemPrompt formats the quoted context for --append-system-prompt,
// instructing the model NOT to echo the quote back.
//
// PRIVACY TRADE-OFF: the string is an argv element, visible via
// /proc/<pid>/cmdline, `ps`, journal snapshots and shim state files. Acceptable
// for single-operator deployments; multi-tenant must use stdin or an env var.
//
// Defense-in-depth: stripArgvControlBytes ensures a NUL (which silently
// truncates the argv element at execve) cannot survive even if a future caller
// skips SanitizeQuote / renderTurnLine.
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
	return stripArgvControlBytes(b.String())
}

// stripArgvControlBytes drops bytes that corrupt argv at execve (NUL truncates
// the argument; bare C0 other than \t \n \r confuses downstream parsers).
// Mirrors config.validateArgvStrings for YAML argv.
func stripArgvControlBytes(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == 0 || (b < 0x20 && b != '\t' && b != '\n' && b != '\r') || b == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == 0 || (b < 0x20 && b != '\t' && b != '\n' && b != '\r') || b == 0x7f {
			continue
		}
		out = append(out, b)
	}
	return string(out)
}

// renderContextTurns serialises user/assistant turns surrounding the quoted
// message into a plain-text block. Turns are filled from the quote outward
// (tail of before, head of after) until adding one would exceed budgetBytes.
// Returns (block, included count, whether any candidate was rejected).
func renderContextTurns(before, after []clievent.EventEntry, budgetBytes int) (string, int, bool) {
	beforeFiltered := filterContextEntries(before)
	afterFiltered := filterContextEntries(after)
	totalCandidates := len(beforeFiltered) + len(afterFiltered)

	if budgetBytes <= 0 {
		// truncated=true when candidates existed so UI / logs can say so.
		return "", 0, totalCandidates > 0
	}
	if totalCandidates == 0 {
		return "", 0, false
	}

	// before is consumed newest-first (tail), after oldest-first (head).
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

	// `used` tracks the actual output length: a join-newline is charged only
	// when content already exists, so N entries cost sum(len) + (N-1), never +N.
	includedBefore := make([]string, 0, len(beforeStack))
	includedAfter := make([]string, 0, len(afterQueue))
	used := 0
	bi, ai := 0, 0
	// Prefer the side with more remaining candidates so extreme imbalance
	// still fills the budget; ties go to `before` (the most recent prior turn
	// is usually more relevant than the next reply).
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

	// includedBefore was collected tail-first; flip back to chronological order.
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

// filterContextEntries keeps only user prompts and assistant text / result
// replies with a non-empty payload. The returned slice is a new allocation.
func filterContextEntries(in []clievent.EventEntry) []clievent.EventEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]clievent.EventEntry, 0, len(in))
	for _, e := range in {
		switch e.Type {
		case "user", "text", "result":
			// Either text (streaming) or result (final envelope) may carry
			// the visible reply, so both are kept.
		default:
			continue
		}
		if pickEntryText(e) == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// renderTurnLine formats one event entry as a role-tagged line. The payload is
// sanitized and capped so a single multi-KB entry cannot eat the whole budget.
func renderTurnLine(e clievent.EventEntry) string {
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

// pickEntryText returns Detail (fuller form) or Summary when Detail is empty.
func pickEntryText(e clievent.EventEntry) string {
	if e.Detail != "" {
		return e.Detail
	}
	return e.Summary
}

// newScratchID returns a 32-char lowercase hex string backed by crypto/rand.
func newScratchID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
