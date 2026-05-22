package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/osutil"
)

// cron-dashboard-redesign P2a §4.4.3 — transcript endpoint.
//
// Goal: surface the assistant/tool/user turn timeline persisted by the
// claude CLI for a given cron run. The CLI writes JSONL into
// ~/.claude/projects/<encode(WorkDir)>/<SessionID>.jsonl already; this
// endpoint streams a *segment* of that file (just the lines whose
// timestamp falls inside the run's [StartedAt, EndedAt] window) and
// flattens each line into a Turn shape the dashboard can render
// without re-implementing JSONL parsing in JS.
//
// Failure model: this endpoint never 5xx's on absent / corrupt data.
// Three downgrade states convey the failure to the client so it can
// fall back to the "原始日志" tab:
//
//	fallback:"missing"  SessionID empty or JSONL not found
//	fallback:"raw"      JSONL exists but no recognised turns parsed
//	truncated:true      hit one of the size caps (8MB/500 turns/etc)
//
// All caps are conservative; the typical cron run produces 5-50 turns
// totalling <100 KB so the limits exist purely as safety bounds against
// pathological JSONL files (long-lived fresh=false sessions accumulate
// turns from many runs into one file — see internal/cron/run.go:48).

const (
	// maxTranscriptBytes is the hard cap on bytes read from the JSONL
	// file. Beyond this we set truncated:true and stop. 8 MB roughly
	// equals 8000 turns at 1 KB each, which is far beyond any realistic
	// single cron run. The cap is conservative because JSONL files in
	// fresh=false mode share state across runs and could otherwise grow
	// without bound.
	maxTranscriptBytes int64 = 8 * 1024 * 1024

	// maxTranscriptTurns caps the number of decoded turns returned. A
	// single cron run rarely produces more than 50-100 turns; the cap
	// guards against pathological prompts that loop tool_use forever.
	maxTranscriptTurns = 500

	// maxTranscriptLineBytes caps a single JSONL line. Beyond this we
	// drop the line (it can't be valid event data — claude CLI writes
	// at most a few hundred KB per assistant turn). bufio.Scanner's
	// default 64 KB buffer is too small for assistant turns with long
	// text + tool_use blocks; we set 256 KB explicitly.
	maxTranscriptLineBytes = 256 * 1024

	// maxToolOutputBytes caps the tool_use_result string we surface to
	// the dashboard. Tool outputs (especially Bash stdout) can be
	// megabytes; the dashboard is a viewer not a log archive.
	maxToolOutputBytes = 32 * 1024

	// maxAssistantTextBytes caps a single assistant text block.
	maxAssistantTextBytes = 64 * 1024
)

// ansiEscRe matches the most common ANSI CSI sequences (color, cursor
// motion). We strip these from tool output before serialising so the
// rendered <pre> doesn't show garbled bytes. Defensive: the dashboard
// uses esc()-then-<pre> so the bytes wouldn't be interpreted as HTML
// either way, but they'd render as literal escape codes which hurt
// readability for a debugging-focused view.
var ansiEscRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// transcriptResponse is the wire shape the dashboard consumes.
type transcriptResponse struct {
	SessionID string            `json:"session_id,omitempty"`
	StartedAt int64             `json:"started_at,omitempty"`
	EndedAt   int64             `json:"ended_at,omitempty"`
	Tokens    *transcriptTokens `json:"tokens,omitempty"`
	ToolCalls int               `json:"tool_calls"`
	Turns     []transcriptTurn  `json:"turns"`
	NextIndex int               `json:"next_index"`
	Truncated bool              `json:"truncated"`
	// Fallback signals a degraded path:
	//   "missing" — SessionID empty or JSONL not found
	//   "raw"     — JSONL exists but no turns parsed
	//   ""        — normal path
	Fallback string `json:"fallback,omitempty"`
}

type transcriptTokens struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	Total  int `json:"total"`
}

// transcriptTurn is a single rendered timeline entry. Only fields
// relevant to its kind are populated; absent fields stay at zero values
// (omitted via json:"omitempty"). Index reflects this turn's position in
// the *response*, not the original JSONL line — the dashboard uses it
// for stable React-style keys when diffing live updates.
type transcriptTurn struct {
	Index      int    `json:"index"`
	Kind       string `json:"kind"` // "user" | "assistant" | "tool_use" | "tool_result" | "error"
	TS         int64  `json:"ts,omitempty"`
	Text       string `json:"text,omitempty"`        // user / assistant / error
	Tokens     int    `json:"tokens,omitempty"`      // assistant only (output token delta)
	Tool       string `json:"tool,omitempty"`        // tool_use
	ToolUseID  string `json:"tool_use_id,omitempty"` // tool_use / tool_result link
	Summary    string `json:"summary,omitempty"`     // tool_use one-liner derived from input
	Input      any    `json:"input,omitempty"`       // tool_use raw input (object)
	Output     string `json:"output,omitempty"`      // tool_result content
	Status     string `json:"status,omitempty"`      // tool_result: "ok" | "error"
	DurationMS int64  `json:"duration_ms,omitempty"` // tool_result duration if available
}

// claudeJSONLEvent is the partial schema we care about. Fields we don't
// use are decoded into RawMessage so a future field addition by the CLI
// doesn't break parsing.
type claudeJSONLEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"`
	UUID      string          `json:"uuid"`
	Message   json.RawMessage `json:"message"`
	// tool_result events sometimes appear at top level under
	// "toolUseResult" instead of inside a content block (varies by
	// CLI version). We tolerate both shapes.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

// claudeMessage is the inner "message" field. Only role + content +
// usage matter to us.
type claudeMessage struct {
	Role    string              `json:"role"`
	Content json.RawMessage     `json:"content"` // string OR []contentBlock
	Usage   *claudeMessageUsage `json:"usage,omitempty"`
}

type claudeMessageUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// claudeContentBlock is one entry in an assistant message's content
// array. The CLI emits these for text / tool_use / tool_result /
// thinking. We surface the first three.
type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`        // type=text
	ID        string          `json:"id,omitempty"`          // type=tool_use
	Name      string          `json:"name,omitempty"`        // type=tool_use
	Input     json.RawMessage `json:"input,omitempty"`       // type=tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"` // type=tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // type=tool_result (string OR array)
	IsError   bool            `json:"is_error,omitempty"`    // type=tool_result
}

// GET /api/cron/runs/{run_id}/transcript?job_id=<jid>
func (h *CronHandlers) handleRunTranscript(w http.ResponseWriter, r *http.Request) {
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	runID, jobID, ok := parseRunPathParams(w, r)
	if !ok {
		return
	}

	run, err := h.scheduler.GetRun(jobID, runID)
	if err != nil {
		if errors.Is(err, cron.ErrCorruptRun) {
			slog.Warn("cron transcript: run record corrupt", "job_id", jobID, "run_id", runID, "err", err)
			http.Error(w, "run record corrupt", http.StatusInternalServerError)
			return
		}
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	// Cross-key check: defensive even though runStore.Get already keys
	// the lookup on the disk path. A future refactor that loosens the
	// key should not silently expose other-job runs through this URL.
	if run.JobID != jobID {
		slog.Warn("cron transcript: job_id mismatch", "url_job_id", jobID, "run_job_id", run.JobID, "run_id", runID)
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	resp := transcriptResponse{
		SessionID: run.SessionID,
		StartedAt: run.StartedAt.UnixMilli(),
		Turns:     []transcriptTurn{},
	}
	if !run.EndedAt.IsZero() {
		resp.EndedAt = run.EndedAt.UnixMilli()
	}

	// Bail early into "missing" downgrade for the common no-session case.
	if run.SessionID == "" || h.claudeDir == "" || run.WorkDir == "" {
		resp.Fallback = "missing"
		writeJSON(w, resp)
		return
	}
	if !discovery.IsValidSessionID(run.SessionID) {
		// Defence in depth: the persisted SessionID *should* be a UUID
		// because session.NewKey enforces it, but a hand-edited disk
		// file could carry path traversal characters. Reject without
		// touching the filesystem at all.
		slog.Warn("cron transcript: skipping non-UUID session_id", "job_id", jobID, "run_id", runID)
		resp.Fallback = "missing"
		writeJSON(w, resp)
		return
	}
	if !filepath.IsAbs(run.WorkDir) {
		// Cron job validation rejects relative WorkDir at write time;
		// guard here too because old persisted runs predate that gate.
		resp.Fallback = "missing"
		writeJSON(w, resp)
		return
	}

	jsonlPath := filepath.Join(h.claudeDir, "projects", discovery.ClaudeProjectSlug(run.WorkDir), run.SessionID+".jsonl")

	// Symlink + path-escape guard. EvalSymlinks resolves any symlink
	// in the chain, then HasPrefix ensures the resolved path still lives
	// under <claudeDir>/projects/. Without this a hostile symlink in
	// the user's claude project dir could redirect us to /etc/shadow.
	resolved, err := filepath.EvalSymlinks(jsonlPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			resp.Fallback = "missing"
			writeJSON(w, resp)
			return
		}
		slog.Warn("cron transcript: evalsymlinks failed", "path", jsonlPath, "err", err)
		resp.Fallback = "missing"
		writeJSON(w, resp)
		return
	}
	// Both the resolved JSONL path AND the claudeDir+projects root must be
	// canonicalised before the prefix check. macOS canonicalises /var to
	// /private/var, and any host where claudeDir contains a symlinked
	// component (Docker bind-mounts, AMI-customised layouts) similarly
	// drifts under EvalSymlinks. Without the symmetric resolve the prefix
	// check rejects every legitimate JSONL on those hosts.
	allowedRoot := filepath.Join(h.claudeDir, "projects")
	resolvedRoot, rrErr := filepath.EvalSymlinks(allowedRoot)
	if rrErr != nil {
		// Fall back to the raw root if EvalSymlinks fails (root may not
		// exist yet on a fresh setup); the strict check still applies.
		resolvedRoot = allowedRoot
	}
	resolvedRoot += string(os.PathSeparator)
	if !strings.HasPrefix(resolved+string(os.PathSeparator), resolvedRoot) {
		slog.Warn("cron transcript: path escape attempt", "raw", jsonlPath, "resolved", resolved, "claudeDir", h.claudeDir, "allowedRoot", resolvedRoot)
		resp.Fallback = "missing"
		writeJSON(w, resp)
		return
	}

	// Lstat to reject non-regular files (FIFO, device, dir-with-name
	// matching). Then open + Fstat for TOCTOU defence: the swap could
	// happen between Lstat and Open, but the post-open Fstat catches
	// it because we re-check the type after the file descriptor is
	// already bound to an inode.
	li, err := os.Lstat(resolved)
	if err != nil {
		resp.Fallback = "missing"
		writeJSON(w, resp)
		return
	}
	if !li.Mode().IsRegular() {
		slog.Warn("cron transcript: non-regular file rejected", "path", resolved, "mode", li.Mode())
		resp.Fallback = "missing"
		writeJSON(w, resp)
		return
	}

	f, err := os.Open(resolved) // #nosec G304 -- path validated above
	if err != nil {
		resp.Fallback = "missing"
		writeJSON(w, resp)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		resp.Fallback = "missing"
		writeJSON(w, resp)
		return
	}

	// Time window: only emit turns whose timestamp falls between
	// run.StartedAt and run.EndedAt. fresh=false runs share a JSONL
	// across many cron invocations; without this filter the response
	// would mix turns from earlier runs.
	startedMS := run.StartedAt.UnixMilli()
	var endedMS int64
	if !run.EndedAt.IsZero() {
		endedMS = run.EndedAt.UnixMilli()
	} else {
		// Running run: include everything up to "now". A small slack
		// (5s) handles clock skew between the cron wall-clock and the
		// JSONL writer (CLI subprocess), neither of which is NTP-synced
		// in test fixtures.
		endedMS = time.Now().UnixMilli() + 5000
	}

	tokens := transcriptTokens{}
	toolCalls := 0

	// LimitReader caps total bytes read; bufio.Scanner with a 256 KB
	// buffer caps single-line bytes. Together they enforce the
	// design's three-tier size budget without ever calling
	// os.ReadFile on the underlying file.
	lr := io.LimitReader(f, maxTranscriptBytes)
	scanner := bufio.NewScanner(lr)
	scanner.Buffer(make([]byte, 0, 64*1024), maxTranscriptLineBytes)

	turns := make([]transcriptTurn, 0, 32)
	truncated := false
	parsedAny := false

	for scanner.Scan() {
		if len(turns) >= maxTranscriptTurns {
			truncated = true
			break
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev claudeJSONLEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Skip unparseable line; do not fail the whole response.
			continue
		}
		// Time-window filter applies only to dated events. The CLI
		// writes "queue-operation" / "attachment" events without
		// timestamps for some shapes; we let those pass through and
		// drop later if they're not turn-worthy.
		ts := parseISO8601MS(ev.Timestamp)
		if ts > 0 {
			if ts < startedMS || ts > endedMS {
				continue
			}
		}
		newTurns, addedTokens, addedToolCalls, isParsed := flattenJSONLEvent(&ev, ts, len(turns))
		if isParsed {
			parsedAny = true
		}
		tokens.Input += addedTokens.Input
		tokens.Output += addedTokens.Output
		toolCalls += addedToolCalls
		for _, t := range newTurns {
			if len(turns) >= maxTranscriptTurns {
				truncated = true
				break
			}
			turns = append(turns, t)
		}
	}
	if err := scanner.Err(); err != nil {
		// Don't 5xx — the prefix we did parse is still useful.
		slog.Warn("cron transcript: scan err (returning partial)", "path", resolved, "err", err)
		truncated = true
	}

	// LimitReader hit means we read maxTranscriptBytes worth without
	// seeing EOF. Mark truncated too.
	if pos, _ := f.Seek(0, io.SeekCurrent); pos >= maxTranscriptBytes {
		truncated = true
	}

	tokens.Total = tokens.Input + tokens.Output
	resp.Turns = turns
	resp.NextIndex = len(turns)
	resp.Truncated = truncated
	resp.ToolCalls = toolCalls
	if tokens.Total > 0 {
		resp.Tokens = &tokens
	}
	if !parsedAny {
		// File existed and was readable but no recognised turns came
		// out. Surface as "raw" so the dashboard switches to the
		// 原始日志 tab instead of showing an empty conversation.
		resp.Fallback = "raw"
	}

	writeJSON(w, resp)
}

// parseRunPathParams extracts run_id (path) + job_id (query) and
// validates both. Centralised so handleRunDetail and handleRunTranscript
// share the exact same gate.
func parseRunPathParams(w http.ResponseWriter, r *http.Request) (runID, jobID string, ok bool) {
	runID = r.PathValue("run_id")
	if runID == "" {
		http.Error(w, "run_id is required", http.StatusBadRequest)
		return "", "", false
	}
	if len(runID) > runIDLenLimit {
		http.Error(w, "run_id too long", http.StatusBadRequest)
		return "", "", false
	}
	if !cron.IsValidID(runID) {
		http.Error(w, "run_id must be lowercase hex", http.StatusBadRequest)
		return "", "", false
	}
	jobID = r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return "", "", false
	}
	if len(jobID) > maxCronIDLenDashboard {
		http.Error(w, "job_id too long", http.StatusBadRequest)
		return "", "", false
	}
	if !cron.IsValidID(jobID) {
		http.Error(w, "job_id must be lowercase hex", http.StatusBadRequest)
		return "", "", false
	}
	return runID, jobID, true
}

// flattenJSONLEvent decodes one JSONL line into 0..N transcript turns.
// Returns (turns, token deltas, tool-call delta, parsedAny).
//
// parsedAny is true when the event maps to at least one recognised turn
// shape — used by the caller to decide whether to set fallback:"raw".
func flattenJSONLEvent(ev *claudeJSONLEvent, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	out := make([]transcriptTurn, 0, 2)
	tok := transcriptTokens{}
	toolCalls := 0
	parsed := false

	switch ev.Type {
	case "user":
		var msg claudeMessage
		if err := json.Unmarshal(ev.Message, &msg); err != nil {
			return out, tok, 0, false
		}
		// content can be a plain string OR a content-block array (when
		// the user message contains a tool_result).
		text, blocks := decodeStringOrBlocks(msg.Content)
		if text != "" {
			parsed = true
			out = append(out, transcriptTurn{
				Index: nextIdx + len(out),
				Kind:  "user",
				TS:    ts,
				Text:  truncateRunes(text, maxAssistantTextBytes),
			})
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				parsed = true
				outStr, _ := decodeStringOrBlocks(b.Content)
				outStr = ansiEscRe.ReplaceAllString(outStr, "")
				outStr = truncateRunes(outStr, maxToolOutputBytes)
				status := "ok"
				if b.IsError {
					status = "error"
				}
				out = append(out, transcriptTurn{
					Index:     nextIdx + len(out),
					Kind:      "tool_result",
					TS:        ts,
					ToolUseID: b.ToolUseID,
					Output:    outStr,
					Status:    status,
				})
			}
		}
	case "assistant":
		var msg claudeMessage
		if err := json.Unmarshal(ev.Message, &msg); err != nil {
			return out, tok, 0, false
		}
		_, blocks := decodeStringOrBlocks(msg.Content)
		if msg.Usage != nil {
			tok.Input = msg.Usage.InputTokens
			tok.Output = msg.Usage.OutputTokens
		}
		// Aggregate text blocks into one assistant turn (multiple text
		// blocks in a single message are common for streamed responses
		// and split awkwardly when shown as separate timeline entries).
		var textBuf strings.Builder
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if textBuf.Len() > 0 {
					textBuf.WriteString("\n\n")
				}
				textBuf.WriteString(b.Text)
			case "tool_use":
				toolCalls++
				summary := summariseToolInput(b.Name, b.Input)
				out = append(out, transcriptTurn{
					Index:     nextIdx + len(out),
					Kind:      "tool_use",
					TS:        ts,
					Tool:      b.Name,
					ToolUseID: b.ID,
					Summary:   summary,
					Input:     json.RawMessage(b.Input),
				})
				parsed = true
			}
		}
		if textBuf.Len() > 0 {
			text := textBuf.String()
			out = append([]transcriptTurn{{
				Index:  nextIdx,
				Kind:   "assistant",
				TS:     ts,
				Text:   truncateRunes(text, maxAssistantTextBytes),
				Tokens: tok.Output,
			}}, out...)
			// re-number subsequent turns
			for i := range out {
				out[i].Index = nextIdx + i
			}
			parsed = true
		}
	case "system":
		// system events (init, error from claude) — surface errors
		// as an error turn so timeline shows them; init is dropped.
		var sys struct {
			Subtype string `json:"subtype"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(ev.Message, &sys)
		if sys.Subtype == "error" && sys.Message != "" {
			out = append(out, transcriptTurn{
				Index: nextIdx,
				Kind:  "error",
				TS:    ts,
				Text:  truncateRunes(sys.Message, maxAssistantTextBytes),
			})
			parsed = true
		}
	}
	return out, tok, toolCalls, parsed
}

// decodeStringOrBlocks accepts either a JSON string or an array of
// content blocks and returns (string-form, blocks-form). One of the
// two is empty depending on what the input was.
func decodeStringOrBlocks(raw json.RawMessage) (string, []claudeContentBlock) {
	if len(raw) == 0 {
		return "", nil
	}
	// Strings are a quoted JSON value starting with `"`.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", nil
		}
		return s, nil
	}
	if raw[0] == '[' {
		var blocks []claudeContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return "", nil
		}
		return "", blocks
	}
	// Object — uncommon; ignore.
	return "", nil
}

// summariseToolInput builds a one-line label for the tool_use card
// header. Best-effort: Bash → command, Read/Write/Edit → file_path,
// otherwise fall back to a JSON-trimmed dump of the input.
func summariseToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return ""
	}
	prefer := []string{"command", "file_path", "path", "url", "pattern", "query"}
	for _, k := range prefer {
		if v, ok := obj[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return osutil.SanitizeForLog(s, 200)
			}
		}
	}
	// Fallback: marshal back, trim to 200 runes.
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return osutil.SanitizeForLog(string(b), 200)
}

// parseISO8601MS converts an RFC 3339 / ISO 8601 timestamp into unix ms.
// Returns 0 when the input is empty or unparseable so callers can use
// it as a fall-through "skip filter" sentinel.
func parseISO8601MS(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}

// truncateRunes caps a string to maxBytes by rune boundary, appending
// an ellipsis indicator when truncation actually happened. We trim by
// rune count rather than byte count so multi-byte UTF-8 sequences don't
// get split mid-codepoint (which would render as U+FFFD in the browser).
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Walk by rune until we cross the byte budget.
	cum := 0
	for i, r := range s {
		_ = r
		if i >= maxBytes-3 {
			return s[:cum] + "…"
		}
		cum = i
	}
	return s
}

// _ keeps strconv imported for future tweaks (line index in errors).
var _ = strconv.Itoa
