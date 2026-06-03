package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"unsafe"
)

// readEventPool recycles the scratch *Event that ReadEvent unmarshals each
// shim stdout frame into. R220123-PERF-13 (#1637): json.Unmarshal forces
// its `&ev` argument to escape to the heap (reflection retains internal
// pointer state into the destination), so the obvious `var ev Event` local
// produced one heap allocation of the ~300-byte Event header per frame —
// 5-50 events/s × N sessions. Pooling the header eliminates that steady
// per-frame allocation. The returned []Event{*ev} carries a value COPY, so
// the pooled struct is safe to reuse the instant ReadEvent returns; its
// nested pointer fields (Message, Metadata, ...) are owned by the copy and
// must be cleared on Put so the pool does not pin a turn's content graph.
//
// This is the *Event header survivor noted as out of scope by
// R222-PERF-3 (#700), which only removed the []byte(line) input copy.
var readEventPool = sync.Pool{New: func() any { return new(Event) }}

// resetEvent zeroes every field of a pooled Event before it re-enters the
// pool so no stale pointer (which would keep a prior frame's AssistantMessage
// / Metadata / RawParams graph alive) or stale scalar leaks into the next
// Unmarshal. A whole-struct assignment is the cheapest correct reset and is
// resilient to new fields being added to Event.
func resetEvent(ev *Event) {
	*ev = Event{}
}

// stringToBytesUnsafe aliases s's backing storage as a []byte without
// allocating. The returned slice MUST be treated as read-only — Go strings
// are immutable, so any mutation (including by the bytes-borrow recipient)
// is undefined behaviour.
//
// Used on the ReadEvent hot path (#700 / R222-PERF-3): json.Unmarshal only
// reads its input buffer, so handing it the aliased bytes saves the
// per-event []byte(line) heap copy that was the dominant survivor on the
// stream-json ingest path. Mirror of the symmetric encode-side trick in
// shim/protocol.go MarshalStdoutLine.
//
// Returns nil for the empty string so callers don't accidentally pass a
// zero-length slice with a nil StringData pointer to json.Unmarshal.
func stringToBytesUnsafe(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// resumeIDRe accepts only characters that can legally appear in a Claude
// session UUID (hex + hyphen). This is a defence-in-depth check at the CLI
// argv boundary — without it, a crafted resume_id beginning with `-` could
// be re-interpreted by the Claude CLI as a flag.
//
// R232-SEC-12: tightened from `[A-Za-z0-9._-]` to `[A-Za-z0-9-]`. Real
// Claude session IDs are UUIDs (36-char hex+hyphen) with neither `.` nor
// `_`; the broader charset was leftover slack that widened the argv
// surface without any legitimate consumer relying on it.
var resumeIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// ClaudeProtocol implements Protocol for Claude CLI's stream-json format.
//
// The spawned claude reads ~/.claude/settings.json directly via
// `--setting-sources user` (see BuildArgs), so naozhi-spawned cc behaves
// identically to a command-line cc: single config source, zero extra
// naozhi-side config, no settings-override copy to maintain. The historical
// `--setting-sources "" + --settings <override>` path (writeClaudeSettingsOverride
// / filterHooks) was removed in docs/rfc/direct-user-settings.md PR1. Hook
// feedback-loop protection now lives at naozhi's HTTP entry auth (webhook
// signing + dashboard token), not by filtering the user's settings file.
type ClaudeProtocol struct{}

func (p *ClaudeProtocol) Name() string { return "stream-json" }

func (p *ClaudeProtocol) Clone() Protocol {
	return &ClaudeProtocol{}
}

func (p *ClaudeProtocol) BuildArgs(opts SpawnOptions) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		// Passthrough matching depends on CLI echoing every stdin user message
		// back as an isReplay:true event with round-tripped uuid. See
		// docs/rfc/passthrough-mode.md §5.3 and validation report V3/V6.
		// Safe to always enable: replay events are filtered out of EventLog
		// (see filterReplayEvent).
		"--replay-user-messages",
		// Load the user's ~/.claude/settings.json directly so naozhi-spawned
		// cc matches command-line cc (single config source, no override copy).
		// docs/rfc/direct-user-settings.md PR1. Note: sysession Runner keeps
		// `--setting-sources ""` (it has no entry-auth and AutoTitler could
		// dead-loop on host hooks — see runner.go).
		"--setting-sources", "user",
	}
	// R215-SEC-P1-1 / #531: --dangerously-skip-permissions used to be
	// hard-coded above. It is required by naozhi's `-p` long-lived process
	// model (headless mode has no interactive prompt surface), so the
	// zero-value PermissionModeDefault keeps emitting it — every existing
	// caller stays bit-identical. Multi-tenant / untrusted deployments opt
	// out per-spawn by setting opts.PermissionMode = PermissionModeStandard,
	// which omits the flag and accepts that the turn will stall on the
	// first permission prompt.
	if opts.PermissionMode == PermissionModeDefault {
		args = append(args, "--dangerously-skip-permissions")
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ResumeID != "" {
		if resumeIDRe.MatchString(opts.ResumeID) {
			args = append(args, "--resume", opts.ResumeID)
		} else {
			// Drop malformed IDs rather than erroring: the caller may
			// have passed a user-facing label and we still want a fresh
			// session. Log at Warn so audit / forensic review can catch
			// argv-injection probes (e.g. ResumeID starting with `-`)
			// instead of silently sliding through. R246-SEC-4 (REPEAT-3
			// with R232-SEC-12 / R245 round): the original "silent drop"
			// behaviour kept getting flagged for lack of an audit trail.
			//
			// Log only the length + a 16-rune prefix so an attacker can't
			// pivot the warning into a log-flooding amplifier (resume IDs
			// are bounded by SpawnOptions but the warn line shouldn't pin
			// arbitrarily-large strings into operator log retention).
			preview := opts.ResumeID
			if len(preview) > 16 {
				preview = preview[:16]
			}
			slog.Warn("cli: --resume rejected by argv validator, spawning fresh session",
				"len", len(opts.ResumeID),
				"prefix", preview)
		}
	}
	args = append(args, capExtraArgsBytes(opts.ExtraArgs)...)
	return args
}

// maxExtraArgsBytes caps the total byte length of opts.ExtraArgs joined. The
// kernel's ARG_MAX is ~2 MiB on Linux; once argv+envp+padding crosses that,
// exec returns E2BIG and the spawn fails opaquely. Realistic ExtraArgs payloads
// (e.g. scratch session --append-system-prompt with 24 KiB quote +
// project-level system prompts) stay well under 128 KiB. Drop the entire slice
// rather than truncating mid-arg, since flag-value pairs cannot be safely cut.
const maxExtraArgsBytes = 128 * 1024

// capExtraArgsBytes guards against a runaway caller (or accumulated stacked
// scratch contexts) producing an argv that exceeds ARG_MAX. After the byte cap
// it also strips dangerous CLI-behaviour-altering flags (R219-SEC-1 / #653)
// that should never be smuggled in via ExtraArgs — a Claude CLI sub-flag
// repurposed by an attacker-controlled prompt or a misconfigured agent could
// mount attacker-supplied MCP servers, expand the file-read sandbox, or
// disable the permission gate. The naozhi spawn pipeline already sets these
// flags itself when needed (--dangerously-skip-permissions in BuildArgs,
// --append-system-prompt by router/scratch via dedicated sites), so any
// occurrence inside ExtraArgs is by definition a duplicate or an injection.
//
// Returns the input unchanged when within the cap and free of disallowed
// flags; logs and returns nil when the byte cap is exceeded; logs and returns
// a filtered copy when only flag stripping is needed.
func capExtraArgsBytes(extra []string) []string {
	total := 0
	for _, a := range extra {
		total += len(a) + 1 // +1 for argv NUL separator
		if total > maxExtraArgsBytes {
			slog.Warn("cli: ExtraArgs exceeds byte cap, dropping",
				"total_bytes", total, "cap", maxExtraArgsBytes, "count", len(extra))
			return nil
		}
	}
	return filterDeniedFlags(extra)
}

// deniedExtraFlags lists Claude/ACP CLI flags that callers must not be able
// to inject through opts.ExtraArgs. The denial covers both the bare form
// (`--name value`) and the equals form (`--name=value`); when the bare form
// fires we also drop the immediately-following element so the orphaned value
// does not slide into argv as a free-standing token.
//
// Allowlist would be safer in principle but a closed enumeration is brittle
// against legitimate operator additions (e.g. `--debug`); the denylist
// approach pins the known-dangerous surface enumerated in R219-SEC-1 +
// R217-SEC-1 while leaving the rest of the CLI's flag surface available.
// Callers needing one of these flags must wire it through a dedicated
// SpawnOptions field that BuildArgs renders explicitly, not the catch-all
// ExtraArgs slice.
var deniedExtraFlags = map[string]struct{}{
	"--mcp-config":                   {}, // loads attacker-controlled MCP server defs
	"--add-dir":                      {}, // expands file-read sandbox
	"--dangerously-skip-permissions": {}, // BuildArgs already controls this
	"--append-system-prompt":         {}, // router/scratch own this site
	"--system-prompt":                {}, // hard override of system prompt
	"--setting-sources":              {}, // BuildArgs pins "user" (load ~/.claude/settings.json)
	"--settings":                     {}, // naozhi no longer injects a settings override file
	"--resume":                       {}, // BuildArgs owns ResumeID validation
	"--allowed-tools":                {}, // permission allowlist override
	"--disallowed-tools":             {}, // permission allowlist override
	"--permission-mode":              {}, // SpawnOptions.PermissionMode owns this
	"--permission-prompt-tool":       {}, // permission gate plumbing
	"--output-format":                {}, // BuildArgs pins stream-json; operator override breaks the NDJSON parser
	"--input-format":                 {}, // same protocol-framing concern
	"--verbose":                      {}, // stream-json verbosity is BuildArgs-controlled
	"--replay-user-messages":         {}, // protocol replay flag owned by BuildArgs
}

// filterDeniedFlags returns extra with any deniedExtraFlags occurrences (and
// their attached values) removed. When nothing is filtered, the input slice
// is returned unchanged so the no-op case avoids the allocation.
func filterDeniedFlags(extra []string) []string {
	// Cheap pre-scan: only allocate when at least one match exists.
	hit := false
	for _, a := range extra {
		if isDeniedFlag(a) {
			hit = true
			break
		}
	}
	if !hit {
		return extra
	}
	out := make([]string, 0, len(extra))
	dropped := 0
	for i := 0; i < len(extra); i++ {
		a := extra[i]
		// `--name=value` form: deny by prefix match before '='.
		if eq := strings.IndexByte(a, '='); eq > 0 && strings.HasPrefix(a, "--") {
			if _, bad := deniedExtraFlags[a[:eq]]; bad {
				dropped++
				continue
			}
		}
		// `--name value` form: deny the flag and skip the following value
		// element if present and not itself a flag (which would imply the
		// next token is another flag and the current one was a boolean).
		if _, bad := deniedExtraFlags[a]; bad {
			dropped++
			if i+1 < len(extra) && !strings.HasPrefix(extra[i+1], "-") {
				i++
				dropped++
			}
			continue
		}
		out = append(out, a)
	}
	if dropped > 0 {
		slog.Warn("cli: ExtraArgs contained denied flags; stripped",
			"dropped", dropped, "kept", len(out))
	}
	return out
}

// isDeniedFlag returns true when a is a denied flag in either bare or
// equals form. Centralised so the pre-scan and the filter loop share the
// same predicate without re-implementing the equals-split.
func isDeniedFlag(a string) bool {
	if !strings.HasPrefix(a, "--") {
		return false
	}
	if eq := strings.IndexByte(a, '='); eq > 0 {
		_, bad := deniedExtraFlags[a[:eq]]
		return bad
	}
	_, bad := deniedExtraFlags[a]
	return bad
}

func (p *ClaudeProtocol) Init(_ *JSONRW, _, _ string) (string, error) {
	return "", nil
}

func (p *ClaudeProtocol) WriteMessage(w io.Writer, text string, images []ImageData) error {
	return p.WriteUserMessageLocked(w, "", text, images, "")
}

// WriteUserMessageLocked writes a user message with optional uuid + priority.
// Caller must already hold Process.shimWMu (see protocol.go interface doc).
//
// Empty uuid / priority are omitted from the JSON (omitempty), so the payload
// is byte-identical to the legacy WriteMessage path when both are empty —
// safe for tests and ACP-backed stream-json paths that never set them.
func (p *ClaudeProtocol) WriteUserMessageLocked(w io.Writer, uuid, text string, images []ImageData, priority string) error {
	msg := NewUserMessageWithMeta(text, images, uuid, priority)
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func (p *ClaudeProtocol) SupportsPriority() bool { return true }
func (p *ClaudeProtocol) SupportsReplay() bool   { return true }

// Capabilities returns the hard-coded Caps for Claude stream-json.
// See RNEW-ARCH-404: opt-in accessor for consumers migrating off
// individual SupportsX() methods.
func (p *ClaudeProtocol) Capabilities() Caps {
	return Caps{Replay: true, Priority: true, SoftInterrupt: false, StreamJSON: true}
}

// The NDJSON payload for an in-band "abort this turn" signal sent via stdin
// is hand-built in WriteInterrupt below (R228-PERF-1). The CLI reacts by
// killing any in-flight tool call (bash children are SIGKILL'd), closing the
// current turn with a `stop_reason=tool_use` or `end_turn` result event, and
// returning to the ready state — without tearing down the session. Verified
// against CLI 2.1.119.
//
// DEADCODE-4 (#1197): the legacy `controlRequestInterrupt` /
// `controlRequestInterruptBody` struct types that used to back this
// envelope via json.Marshal have been retired — the byte-template path
// below is the single source of truth for the interrupt envelope shape.
// New protocol-version variants should pair a typed shape with a real
// caller rather than reintroducing orphan types.

func (p *ClaudeProtocol) WriteInterrupt(w io.Writer, requestID string) error {
	// R228-PERF-1: hand-build the static envelope and only json.Marshal the
	// variable requestID, mirroring the ACP WriteInterrupt fast-path
	// (R226-PERF-9). encoding/json takes a fast-path for plain string values
	// (no struct reflection) and yields a properly escaped JSON string with
	// surrounding quotes — identical to what the previous struct-based
	// Marshal produced for the request_id field.
	idJSON, err := json.Marshal(requestID)
	if err != nil {
		return fmt.Errorf("marshal control_request: %w", err)
	}
	var buf [256]byte
	out := buf[:0]
	out = append(out, `{"type":"control_request","request_id":`...)
	out = append(out, idJSON...)
	out = append(out, `,"request":{"subtype":"interrupt"}}`...)
	out = append(out, '\n')
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("write control_request: %w", err)
	}
	return nil
}

// ReadEvent parses a single CLI stream-json line into zero or more Events.
//
// R67-PERF-1 / R71-PERF-H1 / R227-PERF-1 archive anchor: the `line string`
// signature forces a `[]byte(line)` copy on every event for json.Unmarshal,
// which at 5-50 events/s × N active sessions is real heap churn. A breaking
// change to `ReadEvent(line []byte)` would eliminate the copy by letting
// readLoop hand the unparked bufio.Reader slice straight in — but the same
// readLoop today derives `line` as a string from the shim envelope's
// `shimClientMsg.Line string` field on the cross-process boundary
// (`internal/shim/protocol.go`), so a pure []byte signature only pays off
// once the shim wire format also switches its Line field to
// json.RawMessage. The two changes need to ship together to avoid a
// regression where readLoop just allocates the []byte one frame earlier.
// Re-evaluate when the shim protocol revision bump is on the table; until
// then the per-event `[]byte(line)` copy is the dominant survivor and is
// accepted (~200 B-4 KiB per event, dwarfed by the json.Unmarshal value
// graph it feeds).
func (p *ClaudeProtocol) ReadEvent(line string) ([]Event, bool, error) {
	return p.ReadEventInto(line, nil)
}

// ReadEventInto is the allocation-aware variant of ReadEvent. When buf has
// spare capacity the (single) parsed Event is appended into it, letting the
// readLoop hand in a reused stack-allocated array instead of forcing a fresh
// 1-element backing slice on every frame — R20260603-PERF-10 (#1676). Claude
// stream-json always yields at most one Event per line, so a buf of cap ≥1 is
// never re-grown on the hot path. Passing buf=nil reproduces the original
// allocating behaviour for callers that don't care.
//
// The returned slice always uses buf[:0] as its base, so the caller's array is
// the backing store; callers must not retain the slice beyond the next
// ReadEventInto call sharing the same buf (the readLoop iterates and drops it
// within the same frame).
func (p *ClaudeProtocol) ReadEventInto(line string, buf []Event) ([]Event, bool, error) {
	// R20260527122801-PERF-3 (#1334): substring fast-path skip for the
	// dominant 99%-share frame types — hook_started / hook_response /
	// control_response — before paying for the full Event reflect-unmarshal
	// (~1.2 MB/s heap churn at 50 sessions × 50 events/s otherwise). The
	// frame-type token uniquely identifies these events on the wire so a
	// cheap strings.Contains over the raw line is sufficient; full parse
	// still runs for genuine assistant / user / result frames where the
	// payload is needed downstream.
	//
	// R260528-GO-16: anchor the substring to the JSON key context so a
	// user message containing the literal magic word ("hook_started",
	// "control_response") in its body cannot trigger a false-positive
	// skip. The CLI emits these as `"subtype":"hook_started"` /
	// `"type":"control_response"` so requiring the colon-prefix sticks
	// the match to the JSON key boundary; assistant text content can
	// still mention the word verbatim and round-trips through
	// json.Unmarshal as before.
	// :"hook_ covers both :"hook_started" and :"hook_response" while
	// keeping the colon-anchor that prevents false positives in user text
	// (R260528-GO-16). control_response has no common prefix with hook_ so
	// it is checked separately. [R250531-PERF-9]
	if strings.Contains(line, `:"hook_`) ||
		strings.Contains(line, `:"control_response"`) {
		return nil, false, nil
	}
	// R220123-PERF-13 (#1637): unmarshal into a pooled *Event so the Event
	// header is not heap-allocated per frame. The pooled struct is returned
	// to the pool on EVERY exit path (incl. the skip / cap-error early
	// returns) and reset so it pins no prior frame's pointer graph. The
	// success path copies the value out (`*ev`) into the returned slice
	// before the deferred Put, so callers own an independent Event.
	ev := readEventPool.Get().(*Event)
	defer func() {
		resetEvent(ev)
		readEventPool.Put(ev)
	}()
	// stringToBytesUnsafe avoids the per-event []byte(line) heap copy that
	// the obvious []byte(line) cast would force. json.Unmarshal only reads
	// its input, so aliasing the immutable string's storage is safe.
	// R222-PERF-3 (#700).
	if err := json.Unmarshal(stringToBytesUnsafe(line), ev); err != nil {
		return nil, false, err
	}
	// Defence-in-depth: keep the structural skip in case the substring
	// match misses (e.g. CLI starts emitting the token under a different
	// JSON key).
	if ev.Type == "system" && (ev.SubType == "hook_started" || ev.SubType == "hook_response") {
		return nil, false, nil
	}
	if ev.Type == "control_response" {
		return nil, false, nil
	}
	// R229-SEC-10: cap total content bytes to bound per-event CPU / memory
	// amplification. A tampered CLI could emit a 10 MiB nested JSON event
	// (within shim-line cap) whose Message.Content has megabytes of text
	// across hundreds of blocks — every downstream consumer (EventLog ring,
	// JSONL persist, dashboard fan-out) then pays O(N) work. Drop the event
	// rather than truncate so the dashboard doesn't render half a turn.
	if ev.Message != nil {
		if n := contentBytes(ev.Message); n > maxAssistantMessageContentBytes {
			return nil, false, fmt.Errorf("event content exceeds %d bytes (got %d), dropping",
				maxAssistantMessageContentBytes, n)
		}
	}
	// AskUserQuestion surfacing: in `claude -p` (headless) mode the CLI
	// auto-injects an is_error:true tool_result ~3ms after the tool_use,
	// bailing the model back to a text response inside the same turn
	// (verified in test/e2e/askuser/). We can't intercept that — but we
	// can observe the tool_use and let dispatch render an interactive
	// card so the next user turn carries the chosen option(s). The
	// AskQuestion field rides on the same assistant event so the existing
	// tool_use EventLog entry still flows through unchanged.
	//
	// R234-PERF-16 (#1008): substring-skip on the raw line so the
	// per-block walk only runs for events that actually mention the tool.
	// AskUserQuestion is rare in practice — most assistant events carry
	// only text/thinking blocks. strings.Contains over the raw shim line
	// is single-pass and ~3 orders of magnitude cheaper than the
	// for-block iteration when no AQ tool_use is present.
	if ev.Type == "assistant" && ev.Message != nil &&
		strings.Contains(line, "AskUserQuestion") {
		if aq := extractAskQuestion(ev.Message.Content); aq != nil {
			ev.AskQuestion = aq
		}
	}
	// Copy the value out of the pooled *Event so the caller owns an
	// independent Event; the deferred Put then resets and recycles the
	// pooled header. The nested pointer fields (Message, AskQuestion, ...)
	// are freshly allocated by this frame's Unmarshal and travel with the
	// copy — resetEvent only clears the pooled struct's view of them, not
	// the graph the returned copy points at.
	return append(buf[:0], *ev), ev.Type == "result", nil
}

// askUserQuestionInput matches the `input` field of an AskUserQuestion tool_use
// block. Field tags match the exact keys observed in test/e2e/askuser logs.
type askUserQuestionInput struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// extractAskQuestion returns the AskQuestion payload when the content blocks
// contain a tool_use with name "AskUserQuestion" and valid input.
// Returns nil when no AQ tool_use present or the input fails to decode —
// callers treat nil as "no card to render".
//
// Callers should pre-filter via strings.Contains(rawLine, "AskUserQuestion")
// to avoid running the per-block walk for assistant events that don't
// reference the tool — the cheap substring scan is ~1000× faster than the
// structural iteration when no AQ tool_use is present (R234-PERF-16 / #1008).
func extractAskQuestion(blocks []ContentBlock) *AskQuestion {
	for _, b := range blocks {
		if b.Type != "tool_use" || b.Name != "AskUserQuestion" || len(b.Input) == 0 {
			continue
		}
		var inp askUserQuestionInput
		if err := json.Unmarshal(b.Input, &inp); err != nil {
			// Log at Debug so a CC schema drift (shape evolving away from
			// what test/e2e/askuser validated) is traceable instead of
			// silently producing zero cards. Only log input_len — the raw
			// payload may contain user prompt fragments that don't belong
			// in structured logs.
			slog.Debug("extractAskQuestion: input unmarshal failed",
				"err", err, "input_len", len(b.Input))
			return nil
		}
		if len(inp.Questions) == 0 {
			return nil
		}
		items := make([]AskQuestionItem, 0, len(inp.Questions))
		for _, q := range inp.Questions {
			opts := make([]AskQuestionOpt, 0, len(q.Options))
			for _, o := range q.Options {
				opts = append(opts, AskQuestionOpt{Label: o.Label, Description: o.Description})
			}
			items = append(items, AskQuestionItem{
				Question:    q.Question,
				Header:      q.Header,
				MultiSelect: q.MultiSelect,
				Options:     opts,
			})
		}
		return &AskQuestion{ToolUseID: b.ID, Items: items}
	}
	return nil
}

func (p *ClaudeProtocol) HandleEvent(_ io.Writer, _ Event) bool {
	return false
}
