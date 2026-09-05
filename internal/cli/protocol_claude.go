package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unsafe"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/osutil"
)

// readEventPool recycles the scratch *Event that ReadEvent unmarshals each
// shim stdout frame into (#1637): json.Unmarshal forces `&ev` to escape, so a
// plain local heap-allocated the ~300-byte Event header on every frame. The
// returned []Event{*ev} is a value COPY, so the pooled struct is safe to reuse
// the instant ReadEvent returns; its nested pointer fields are owned by the
// copy and must be cleared on Put so the pool does not pin a turn's content.
var readEventPool = sync.Pool{New: func() any { return new(Event) }}

// resetEvent zeroes a pooled Event before it re-enters the pool so no stale
// pointer keeps a prior frame's Message / Metadata / RawParams graph alive.
// Whole-struct assignment is the cheapest correct reset and survives new fields.
func resetEvent(ev *Event) {
	*ev = Event{}
}

// stringToBytesUnsafe aliases s's backing storage as a []byte without
// allocating. The returned slice MUST be treated as read-only — Go strings are
// immutable, so any mutation is undefined behaviour. Used on the ReadEvent hot
// path (#700): json.Unmarshal only reads its input, so aliasing saves the
// per-event []byte(line) copy (mirror of shim/protocol.go MarshalStdoutLine).
// Returns nil for "" so callers never pass a slice with a nil StringData pointer.
func stringToBytesUnsafe(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// resumeIDRe accepts only characters that can legally appear in a Claude
// session UUID (hex + hyphen). Defence-in-depth at the CLI argv boundary:
// without it, a crafted resume_id beginning with `-` could be re-interpreted
// by the Claude CLI as a flag.
var resumeIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// ClaudeProtocol implements Protocol for Claude CLI's stream-json format.
//
// The spawned claude reads ~/.claude/settings.json directly via
// `--setting-sources user` (see BuildArgs), so naozhi-spawned cc behaves
// identically to a command-line cc. Hook feedback-loop protection lives at
// naozhi's HTTP entry auth (webhook signing + dashboard token), not in a
// filtered copy of the user's settings file.
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
		// Passthrough matching depends on the CLI echoing every stdin user message
		// as an isReplay:true event with round-tripped uuid (passthrough-mode.md
		// §5.3). Safe to always enable: replay events are filtered out of EventLog.
		"--replay-user-messages",
	}
	// Settings source: default `--setting-sources user` loads the operator's
	// ~/.claude/settings.json (docs/rfc/direct-user-settings.md); a naozhi-owned
	// SettingsFile instead gets `--setting-sources "" --settings <file>`. The
	// path must be absolute with no leading '-' (argv-injection guard); a bad
	// value falls back to `user` rather than spawning with no settings.
	if opts.SettingsFile != "" && filepath.IsAbs(opts.SettingsFile) && !strings.HasPrefix(opts.SettingsFile, "-") {
		args = append(args, "--setting-sources", "", "--settings", opts.SettingsFile)
	} else {
		args = append(args, "--setting-sources", "user")
	}
	// --dangerously-skip-permissions is required by naozhi's `-p` long-lived
	// process model (headless mode has no interactive prompt surface), so the
	// zero-value PermissionModeDefault emits it. Multi-tenant / untrusted
	// deployments opt out per-spawn via PermissionModeStandard and accept that
	// the turn stalls on the first permission prompt (#531).
	if opts.PermissionMode == PermissionModeDefault {
		args = append(args, "--dangerously-skip-permissions")
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	// `--effort <low|medium|high|xhigh|max>` (Claude CLI ≥ 2.1.226) is a session
	// pin that also overrides settings.json `effortLevel`, so this flag — not a
	// settings key — is the tier's source of truth. Config validation pins the
	// same closed set (config.validEffortTiers); empty means no flag.
	// SpawnOptions.Effort owns this argv site; the flag stays in deniedExtraFlags.
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if opts.ResumeID != "" {
		if resumeIDRe.MatchString(opts.ResumeID) {
			args = append(args, "--resume", opts.ResumeID)
		} else {
			// Drop malformed IDs rather than erroring so a user-facing label
			// still yields a fresh session, but Warn so argv-injection probes
			// (ResumeID starting with `-`) leave an audit trail. Log only the
			// length + a 16-rune prefix so the warning cannot be turned into
			// a log-flooding amplifier.
			preview := opts.ResumeID
			if len(preview) > 16 {
				preview = preview[:16]
			}
			slog.Warn("cli: --resume rejected by argv validator, spawning fresh session",
				"len", len(opts.ResumeID),
				"prefix", preview)
		}
	}
	// Operator-opt-in CLI debug capture: the raw API request/response log is
	// the only place Bedrock retry status codes (429 vs 5xx) are observable. No
	// category filter is passed — it only scopes stderr, never the file. Reject
	// a leading '-' (argv-injection guard) and require an ABSOLUTE path: the CLI
	// runs with cmd.Dir = workspace, so a relative path leaks API keys there (#2133).
	if opts.DebugFile != "" && !strings.HasPrefix(opts.DebugFile, "-") && filepath.IsAbs(opts.DebugFile) {
		args = append(args, "--debug-file", opts.DebugFile)
	}
	// Operator-opt-in MCP server set (RFC cli-mcp-config). `--setting-sources ""`
	// suppresses ~/.claude.json's mcpServers block, so `--mcp-config` is the only
	// injection point for such a spawn; independent of which settings branch ran
	// so the two knobs compose. The flag stays in deniedExtraFlags — this field
	// is the escape hatch. Same absolute-path + no-leading-dash guard as above.
	if opts.MCPConfigFile != "" && !strings.HasPrefix(opts.MCPConfigFile, "-") && filepath.IsAbs(opts.MCPConfigFile) {
		args = append(args, "--mcp-config", opts.MCPConfigFile)
	}
	// naozhi-owned system prompt (#2493). `--append-system-prompt` stays in
	// deniedExtraFlags; this dedicated field is rendered OUTSIDE the ExtraArgs
	// filter so the planner / scratch / agents[].system_prompt channels reach
	// the CLI. Leading-dash, NUL and byte-cap checks (invalidAppendSystemPrompt);
	// an invalid value is dropped whole and logged, never truncated.
	if opts.AppendSystemPrompt != "" {
		if reason := invalidAppendSystemPrompt(opts.AppendSystemPrompt); reason == "" {
			args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
		} else {
			slog.Warn("cli: AppendSystemPrompt rejected by argv validator, spawning without it",
				"reason", reason, "len", len(opts.AppendSystemPrompt))
		}
	}
	args = append(args, capExtraArgsBytes(opts.ExtraArgs)...)
	return args
}

// invalidAppendSystemPrompt returns "" when p may be emitted as the value of
// `--append-system-prompt`, otherwise a short reason suitable for a log attr.
// Callers already sanitise at their own layer; this is the argv-boundary
// backstop shared by every path, so a caller that skips its own check still
// cannot corrupt argv.
func invalidAppendSystemPrompt(p string) string {
	switch {
	case strings.HasPrefix(p, "-"):
		return "leading dash"
	case strings.IndexByte(p, 0) >= 0:
		return "NUL byte"
	case len(p) > MaxAppendSystemPromptBytes:
		return "exceeds byte cap"
	}
	return ""
}

// maxExtraArgsBytes caps the total byte length of opts.ExtraArgs joined. The
// kernel's ARG_MAX is ~2 MiB on Linux; past it exec returns E2BIG and the spawn
// fails opaquely. Realistic payloads (cli.args / agents[].args) stay far under
// 128 KiB; the system prompt has its own MaxAppendSystemPromptBytes budget.
// Drop the entire slice rather than truncating mid-arg, since flag-value pairs
// cannot be safely cut.
const maxExtraArgsBytes = 128 * 1024

// IsDeniedExtraFlag reports whether a single argv token is one of the
// deniedExtraFlags (bare `--name` or `--name=value` form) that BuildArgs
// strips from SpawnOptions.ExtraArgs. Exported so the config loader can tell
// an operator at load time — with the field path — instead of the flag
// vanishing at spawn behind a generic warning (#2493).
func IsDeniedExtraFlag(arg string) bool { return isDeniedFlag(arg) }

// capExtraArgsBytes guards against a runaway caller (or stacked scratch
// contexts) producing an argv that exceeds ARG_MAX, then strips CLI-behaviour-
// altering flags (#653) that must never be smuggled in via ExtraArgs — an
// attacker-controlled prompt or misconfigured agent could otherwise mount
// attacker-supplied MCP servers, expand the file-read sandbox, or disable the
// permission gate. BuildArgs sets those flags itself, so any occurrence here
// is a duplicate or an injection. Returns the input unchanged when clean;
// nil (logged) when over the byte cap; a filtered copy when flags were stripped.
func capExtraArgsBytes(extra []string) []string {
	if _, over := extraArgsOverCap(extra); over {
		return nil
	}
	return filterDeniedFlags(extra)
}

// extraArgsOverCap reports whether extra exceeds maxExtraArgsBytes, returning
// the running byte total at the point of overflow. Shared by the argv gate
// and SpawnDiagsFor so the drop and its diagnostic cannot disagree.
func extraArgsOverCap(extra []string) (int, bool) {
	total := 0
	for _, a := range extra {
		total += len(a) + 1 // +1 for argv NUL separator
		if total > maxExtraArgsBytes {
			return total, true
		}
	}
	return total, false
}

// deniedExtraFlags lists Claude/ACP CLI flags that callers must not inject
// through opts.ExtraArgs, in both bare (`--name value`) and equals
// (`--name=value`) form; when the bare form fires the following element is
// dropped too so the orphaned value does not slide into argv. An allowlist
// would be safer in principle but brittle against legitimate operator flags
// (e.g. `--debug`); the denylist pins the known-dangerous surface. Callers
// needing one of these flags must wire it through a dedicated SpawnOptions
// field that BuildArgs renders explicitly, not the catch-all ExtraArgs slice.
var deniedExtraFlags = map[string]struct{}{
	"--mcp-config":                   {}, // loads attacker-controlled MCP server defs
	"--add-dir":                      {}, // expands file-read sandbox
	"--dangerously-skip-permissions": {}, // BuildArgs already controls this
	"--append-system-prompt":         {}, // SpawnOptions.AppendSystemPrompt owns this site (#2493)
	"--system-prompt":                {}, // hard override of system prompt
	"--setting-sources":              {}, // BuildArgs pins "user" (load ~/.claude/settings.json)
	"--settings":                     {}, // BuildArgs owns SpawnOptions.SettingsFile
	"--resume":                       {}, // BuildArgs owns ResumeID validation
	"--allowed-tools":                {}, // permission allowlist override
	"--disallowed-tools":             {}, // permission allowlist override
	"--model":                        {}, // SpawnOptions.Model owns model selection
	"--effort":                       {}, // SpawnOptions.Effort owns the tier; config validates a closed set
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
	for i := 0; i < len(extra); i++ {
		a := extra[i]
		// `--name=value` form: deny by prefix match before '='.
		if eq := strings.IndexByte(a, '='); eq > 0 && strings.HasPrefix(a, "--") {
			if _, bad := deniedExtraFlags[a[:eq]]; bad {
				continue
			}
		}
		// `--name value` form: deny the flag and skip the following value element
		// unless it is itself a flag (then the current one was a boolean).
		if _, bad := deniedExtraFlags[a]; bad {
			if i+1 < len(extra) && !strings.HasPrefix(extra[i+1], "-") {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	// The strip itself is silent: real spawn paths report it through
	// SpawnDiagsFor + EmitSpawnDiags (with the session key as scope), so the
	// 30s shim-reconcile heartbeat re-deriving argv cannot spam the log.
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

func (p *ClaudeProtocol) WriteMessage(w io.Writer, text string, images []Attachment) error {
	return p.WriteUserMessageLocked(w, "", text, images, "")
}

// userMsgEnc bundles a *bytes.Buffer + *json.Encoder so WriteUserMessageLocked
// encodes each user message into a pooled buffer instead of paying a fresh
// json.Marshal encodeState + output []byte allocation per send (#1826). Unlike
// process_shim_io.go's shimSendBufPool, this encoder keeps HTML escaping ON so
// the bytes match json.Marshal(msg) exactly; the shim pool's encoder disables
// it and would change the wire payload for '<', '>' or '&'.
type userMsgEnc struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

// userMsgEncPool recycles encoder/buffer pairs; the Encoder holds the buffer
// by pointer, so a Reset between uses is safe.
var userMsgEncPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		// HTML escaping ON so the output matches encoding/json.Marshal byte-for-byte.
		enc := json.NewEncoder(buf)
		return &userMsgEnc{buf: buf, enc: enc}
	},
}

// userMsgEncMaxCap caps the pooled buffer capacity: image-bearing messages
// (base64 thumbnails up to ~400KB) inflate it, and oversized entries are
// dropped on Put. Mirrors shimSendBufMaxCap / acpEncBufMaxCap.
const userMsgEncMaxCap = 64 * 1024

func putUserMsgEnc(e *userMsgEnc) {
	if e.buf.Cap() > userMsgEncMaxCap {
		return
	}
	userMsgEncPool.Put(e)
}

// WriteUserMessageLocked writes a user message with optional uuid + priority.
// Caller must already hold Process.shimWMu (see protocol.go interface doc).
// Empty uuid / priority are omitted (omitempty), so the payload is identical to
// the plain WriteMessage path when both are empty.
func (p *ClaudeProtocol) WriteUserMessageLocked(w io.Writer, uuid, text string, images []Attachment, priority string) error {
	msg := NewUserMessageWithMeta(text, images, uuid, priority)
	// Pooled json.Encoder + bytes.Buffer instead of json.Marshal (#1826).
	// Encode appends the trailing '\n' itself, so the NDJSON framing is intact.
	em := userMsgEncPool.Get().(*userMsgEnc)
	em.buf.Reset()
	if err := em.enc.Encode(msg); err != nil {
		// A failed Encode may leave the encoder/buffer undefined; drop the entry
		// rather than returning it to the pool (matches encodeShimMsg).
		return err
	}
	_, err := w.Write(em.buf.Bytes())
	putUserMsgEnc(em)
	return err
}

func (p *ClaudeProtocol) SupportsPriority() bool { return true }
func (p *ClaudeProtocol) SupportsReplay() bool   { return true }

// Capabilities returns the hard-coded Caps for Claude stream-json.
// EffortTier=true: BuildArgs forwards SpawnOptions.Effort as `--effort <tier>`
// (Claude CLI ≥ 2.1.226).
func (p *ClaudeProtocol) Capabilities() Caps {
	return Caps{Replay: true, Priority: true, SoftInterrupt: false, StreamJSON: true,
		EffortTier: true}
}

// WriteInterrupt writes the in-band "abort this turn" control_request. The CLI
// kills any in-flight tool call (bash children are SIGKILL'd), closes the turn
// with a `stop_reason=tool_use` or `end_turn` result event, and returns to
// ready without tearing down the session (verified against CLI 2.1.119). The
// byte template below is the single source of truth for the envelope shape;
// new variants should pair a typed shape with a real caller (#1197).
func (p *ClaudeProtocol) WriteInterrupt(w io.Writer, requestID string) error {
	// appendJSONStringBytes quotes requestID straight into a stack buffer (no
	// json.Marshal heap alloc); it escapes `"`, `\`, C0 controls and
	// U+2028/U+2029 — identical to json.Marshal for a UUID string.
	var buf [256]byte
	out := buf[:0]
	out = append(out, `{"type":"control_request","request_id":`...)
	out = appendJSONStringBytes(out, []byte(requestID))
	out = append(out, `,"request":{"subtype":"interrupt"}}`...)
	out = append(out, '\n')
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("write control_request: %w", err)
	}
	return nil
}

// WriteSetModel writes a set_model control_request to switch the live
// session's model (verified on claude 2.1.251,
// docs/rfc/dashboard-model-effort-control.md §1): a success ack is
// control_response {subtype:"success"} and may land mid-turn, affecting the
// CURRENT turn's remaining output without interrupting it; a rejection ack
// {subtype:"error", error:"…"} covers org-policy-restricted and unknown
// models (the CLI self-validates). The switch persists in claude's own session
// state, but callers still re-apply --model on respawn so kiro/claude match.
func (p *ClaudeProtocol) WriteSetModel(w io.Writer, requestID, model string) error {
	var buf [320]byte
	out := buf[:0]
	out = append(out, `{"type":"control_request","request_id":`...)
	out = appendJSONStringBytes(out, []byte(requestID))
	out = append(out, `,"request":{"subtype":"set_model","model":`...)
	out = appendJSONStringBytes(out, []byte(model))
	out = append(out, `}}`...)
	out = append(out, '\n')
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("write set_model control_request: %w", err)
	}
	return nil
}

// controlResponseFrame is the subset of the CLI's control_response the ack router consumes.
type controlResponseFrame struct {
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
	} `json:"response"`
}

// parseControlAck converts a control_response line into a control_ack Event.
// Returns ok=false for frames without a request_id (nothing to correlate, e.g.
// the bare `{"type":"control_response"}` shape) or that fail to parse; both
// degrade to a skip so a malformed control frame can never kill the readLoop.
// Error text is sanitized: it originates from the CLI (separate trust
// boundary) and flows into slog attrs + the dashboard toast.
func parseControlAck(line string) (Event, bool) {
	var frame controlResponseFrame
	if err := json.Unmarshal(stringToBytesUnsafe(line), &frame); err != nil {
		return Event{}, false
	}
	if frame.Response.RequestID == "" {
		return Event{}, false
	}
	ev := Event{
		Type:         "control_ack",
		SubType:      frame.Response.Subtype,
		RPCRequestID: frame.Response.RequestID,
	}
	if frame.Response.Error != "" {
		ev.Result = osutil.SanitizeForLog(frame.Response.Error, 256)
	}
	return ev, true
}

// ReadEvent parses a single CLI stream-json line into zero or more Events.
// The `line string` signature is kept because readLoop derives line from the
// shim envelope's `shimClientMsg.Line string` (internal/shim/protocol.go); a
// []byte signature only pays off once that wire field becomes json.RawMessage
// too. stringToBytesUnsafe removes the copy in the meantime.
func (p *ClaudeProtocol) ReadEvent(line string) ([]Event, bool, error) {
	return p.ReadEventInto(line, nil)
}

// ReadEventInto is the allocation-aware variant of ReadEvent (#1676). When buf
// has spare capacity the (single) parsed Event is appended into it, so the
// readLoop can hand in a reused array instead of forcing a fresh 1-element
// backing slice per frame; claude stream-json yields at most one Event per
// line, so a buf of cap ≥1 is never re-grown. buf=nil allocates as before.
// The returned slice uses buf[:0] as its base, so callers must not retain it
// beyond the next ReadEventInto call sharing the same buf.
func (p *ClaudeProtocol) ReadEventInto(line string, buf []Event) ([]Event, bool, error) {
	// Fast-path skip for the dominant hook_started / hook_response frames before
	// the full reflect-unmarshal (#1334). The `:"` anchor pins the match to a
	// JSON key so user text containing the word cannot trigger a false skip.
	// control_response frames are rare (one per interrupt / set_model) and carry
	// the ack Process.SetModel blocks on, so they get a targeted parse instead.
	if strings.Contains(line, `:"hook_`) {
		return nil, false, nil
	}
	if strings.Contains(line, `:"control_response"`) {
		if ev, ok := parseControlAck(line); ok {
			return append(buf[:0], ev), false, nil
		}
		return nil, false, nil
	}
	// Pooled *Event so the header is not heap-allocated per frame (#1637). It is
	// returned to the pool on EVERY exit path and reset so it pins no prior
	// frame's pointer graph; the success path copies the value out first.
	ev := readEventPool.Get().(*Event)
	defer func() {
		resetEvent(ev)
		readEventPool.Put(ev)
	}()
	// Aliased bytes: json.Unmarshal only reads its input (#700).
	if err := json.Unmarshal(stringToBytesUnsafe(line), ev); err != nil {
		return nil, false, err
	}
	// Defence-in-depth: structural skip in case the substring match misses
	// (e.g. the CLI starts emitting the token under a different JSON key).
	if ev.Type == "system" && (ev.SubType == "hook_started" || ev.SubType == "hook_response") {
		return nil, false, nil
	}
	if ev.Type == "control_response" {
		return nil, false, nil
	}
	// Cap total content bytes to bound per-event CPU / memory amplification: a
	// tampered CLI could emit a 10 MiB nested event (within the shim-line cap)
	// that every downstream consumer (EventLog ring, JSONL persist, dashboard
	// fan-out) then pays O(N) for. Drop rather than truncate so the dashboard
	// never renders half a turn.
	if ev.Message != nil {
		if n := contentBytes(ev.Message); n > maxAssistantMessageContentBytes {
			return nil, false, fmt.Errorf("event content exceeds %d bytes (got %d), dropping",
				maxAssistantMessageContentBytes, n)
		}
	}
	// AskUserQuestion surfacing: in `claude -p` the CLI auto-injects an
	// is_error tool_result ~3ms after the tool_use and the model falls back to
	// text in the same turn (test/e2e/askuser/). We observe the tool_use so
	// dispatch can render an interactive card. The raw-line substring check
	// keeps the per-block walk off the common path (#1008).
	if ev.Type == "assistant" && ev.Message != nil &&
		strings.Contains(line, "AskUserQuestion") {
		if aq := extractAskQuestion(ev.Message.Content); aq != nil {
			ev.AskQuestion = aq
		}
	}
	// Copy the value out so the caller owns an independent Event; the deferred
	// Put resets only the pooled struct's view, not the freshly-unmarshalled
	// graph (Message, AskQuestion, ...) the copy points at.
	return append(buf[:0], *ev), ev.Type == "result", nil
}

// askUserQuestionInput matches the `input` of an AskUserQuestion tool_use
// block (keys as observed in test/e2e/askuser logs).
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
// contain a tool_use named "AskUserQuestion" with valid input; nil means "no
// card to render". Callers should pre-filter via strings.Contains(rawLine,
// "AskUserQuestion") — the substring scan is ~1000× cheaper than the
// structural walk when no AQ tool_use is present (#1008).
func extractAskQuestion(blocks []ContentBlock) *clievent.AskQuestion {
	for _, b := range blocks {
		if b.Type != "tool_use" || b.Name != "AskUserQuestion" || len(b.Input) == 0 {
			continue
		}
		var inp askUserQuestionInput
		if err := json.Unmarshal(b.Input, &inp); err != nil {
			// Debug-log so a CC schema drift is traceable instead of silently
			// producing zero cards. Only input_len — the raw payload may contain
			// user prompt fragments that don't belong in structured logs.
			slog.Debug("extractAskQuestion: input unmarshal failed",
				"err", err, "input_len", len(b.Input))
			return nil
		}
		if len(inp.Questions) == 0 {
			return nil
		}
		items := make([]clievent.AskQuestionItem, 0, len(inp.Questions))
		for _, q := range inp.Questions {
			opts := make([]clievent.AskQuestionOpt, 0, len(q.Options))
			for _, o := range q.Options {
				opts = append(opts, clievent.AskQuestionOpt{Label: o.Label, Description: o.Description})
			}
			items = append(items, clievent.AskQuestionItem{
				Question:    q.Question,
				Header:      q.Header,
				MultiSelect: q.MultiSelect,
				Options:     opts,
			})
		}
		return &clievent.AskQuestion{ToolUseID: b.ID, Items: items}
	}
	return nil
}

func (p *ClaudeProtocol) HandleEvent(_ io.Writer, _ Event) bool {
	return false
}
