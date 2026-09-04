package cron

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
	"strings"
	"time"
	"unicode/utf8"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Transcript endpoint: surfaces the assistant/tool/user turn timeline the
// claude CLI persists as JSONL under ~/.claude/projects/<slug>/<SessionID>.jsonl,
// streaming only the segment inside the run's [StartedAt, EndedAt] window and
// flattening each line into a Turn the dashboard renders without parsing JSONL.
//
// Failure model: never 5xx on absent / corrupt data. Downgrade states let the
// client fall back to the "原始日志" tab:
//
//	fallback:"missing"  SessionID empty or JSONL not found
//	fallback:"raw"      JSONL exists but no recognised turns parsed
//	truncated:true      hit one of the size caps

const (
	// maxTranscriptBytes is the hard cap on bytes read from the JSONL file;
	// beyond it truncated:true is set. fresh=false JSONL files share state
	// across runs and could otherwise grow without bound.
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

	// maxToolInputBytes caps the raw tool_use.Input JSON we surface. Without it
	// 500 turns × 256KB lines could push ~128MB per (auth'd) request — a trivial
	// memory amplifier. Oversize inputs get a "[truncated]" placeholder so the
	// timeline still renders the call.
	maxToolInputBytes = 64 * 1024

	// summariseInputCap bounds the tool_use.Input bytes fed to json.Unmarshal
	// in summariseToolInput. The probe only needs a few KB to find a one-line
	// label, so capping well below maxToolInputBytes shrinks the worst-case
	// unmarshal fan-out (500 turns × transcriptSem 8) of attacker-influenced
	// JSON; oversize inputs are rejected before parsing (#645, #1584).
	summariseInputCap = 16 * 1024

	// transcriptRunningSlackMS is added to "now" as the window upper bound for
	// a still-running run (no EndedAt yet), absorbing clock skew between the
	// cron wall-clock and the JSONL writer (CLI subprocess) so a turn slightly
	// ahead of "now" still appears in the live view.
	transcriptRunningSlackMS int64 = 5_000
)

// The concurrency gate for HandleRunTranscript is Handlers.transcriptSem
// (cronTranscriptSemCap=8, wired by server/build_handlers.go): each in-flight
// transcript holds a 256 KB scanner buffer plus an 8 MB LimitReader budget, so
// a process-wide ceiling bounds the memory amplifier under multi-operator load.

// truncatedToolInputPlaceholder is the JSON value substituted for
// tool_use.Input fields that exceed maxToolInputBytes. Pre-encoded so the
// hot path never re-marshals; must be a valid JSON value (a string
// literal here) so the wire shape stays consistent for dashboard JS.
var truncatedToolInputPlaceholder = json.RawMessage(`"[truncated]"`)

// ansiEscRe matches common ANSI CSI sequences (color, cursor motion) AND OSC
// sequences (e.g. hyperlink `\x1b]8;;url\x1b\\` / BEL-terminated `\x1b]8;;url\x07`,
// emitted by gh / ls --hyperlink). Stripped from tool output before
// serialising so the rendered <pre> doesn't show garbled escape codes; the
// dashboard's esc()-then-<pre> already prevents HTML interpretation (#788).
var ansiEscRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

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
	// TruncateReason discriminates why Truncated is true so forensics can tell
	// a size-cap hit from a disk read error or an over-long line (#1049).
	// Only populated when Truncated is true:
	//   "size_cap"       — hit maxTranscriptBytes / maxTranscriptTurns
	//   "line_too_long"  — bufio.ErrTooLong (line > maxTranscriptLineBytes)
	//   "scan_io_error"  — Scanner.Err returned a non-ErrTooLong error
	TruncateReason string `json:"truncate_reason,omitempty"`
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

// transcriptTurn is a single rendered timeline entry. Only fields relevant to
// its kind are populated (omitempty). Index is the position in the *response*,
// not the JSONL line — the dashboard uses it as a stable key for live diffs.
//
// CLIENT-SIDE CONTRACT (#921): Input is forwarded as raw JSON bytes from the
// CLI's tool_use payload and httputil.WriteJSON disables SetEscapeHTML, so
// `<`, `>`, `&` survive verbatim. Tool input is attacker-influenced (a
// malicious project file can steer the CLI's tool calls), so any consumer
// must render it via JSON.stringify + esc() or DOMPurify — never raw
// innerHTML. maxToolInputBytes bounds its size but does not normalise bytes.
type transcriptTurn struct {
	Index      int             `json:"index"`
	Kind       string          `json:"kind"` // "user" | "assistant" | "tool_use" | "tool_result" | "error"
	TS         int64           `json:"ts,omitempty"`
	Text       string          `json:"text,omitempty"`        // user / assistant / error
	Tokens     int             `json:"tokens,omitempty"`      // assistant only (output token delta)
	Tool       string          `json:"tool,omitempty"`        // tool_use
	ToolUseID  string          `json:"tool_use_id,omitempty"` // tool_use / tool_result link
	Summary    string          `json:"summary,omitempty"`     // tool_use one-liner derived from input
	Input      json.RawMessage `json:"input,omitempty"`       // tool_use raw input (object) — see CLIENT-SIDE CONTRACT godoc
	Output     string          `json:"output,omitempty"`      // tool_result content
	Status     string          `json:"status,omitempty"`      // tool_result: "ok" | "error"
	DurationMS int64           `json:"duration_ms,omitempty"` // tool_result duration if available
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
func (h *Handlers) HandleRunTranscript(w http.ResponseWriter, r *http.Request) {
	// Use the dedicated transcriptLimiter rather than the shared runsLimiter:
	// the transcript path fans out far more I/O (EvalSymlinks ×2 + 8 MB
	// LimitReader + 256 KB scanner + per-line Unmarshal), so one shared bucket
	// would let either endpoint starve the other (#1096). runsLimiter is the
	// fallback for hand-rolled fixtures without a transcriptLimiter.
	limiter := h.transcriptLimiter
	if limiter == nil {
		limiter = h.runsLimiter
	}
	if limiter != nil && !limiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron transcript rate limit exceeded"})
		return
	}
	// Cap concurrent in-flight transcript reads: each holds 256 KB of scanner
	// buffer plus an 8 MB LimitReader budget, so N operators could park N×8 MB
	// without this gate (#798). Non-blocking acquire fails "503 immediately"
	// rather than slow-loris holding a goroutine. Acquired BEFORE the scheduler
	// nil check so the gate is testable in isolation; nil-guarded for fixtures.
	if h.transcriptSem != nil {
		select {
		case h.transcriptSem <- struct{}{}:
			defer func() { <-h.transcriptSem }()
		case <-r.Context().Done():
			httputil.WriteJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "transcript busy"})
			return
		default:
			httputil.WriteJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "transcript busy"})
			return
		}
	}
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	runID, jobID, ok := parseRunPathParams(w, r)
	if !ok {
		return
	}

	run, err := h.scheduler.Run(jobID, runID)
	if err != nil {
		if errors.Is(err, cronpkg.ErrCorruptRun) {
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
		httputil.WriteJSON(w, resp)
		return
	}
	if !discovery.IsValidSessionID(run.SessionID) {
		// Defence in depth: the persisted SessionID *should* be a UUID
		// because session.NewKey enforces it, but a hand-edited disk
		// file could carry path traversal characters. Reject without
		// touching the filesystem at all.
		slog.Warn("cron transcript: skipping non-UUID session_id", "job_id", jobID, "run_id", runID)
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}
	if !filepath.IsAbs(run.WorkDir) {
		// Cron job validation rejects relative WorkDir at write time;
		// guard here too because old persisted runs predate that gate.
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}
	// Defence in depth before ClaudeProjectSlug encodes WorkDir into a path
	// component: the slug only maps '/'→'-' and does NOT scrub control runes
	// or invalid UTF-8, so a hand-edited or legacy persisted run could tunnel
	// a control rune into the projects/ directory name and steer EvalSymlinks
	// onto an unintended path. Reject before constructing jsonlPath.
	if !utf8.ValidString(run.WorkDir) {
		slog.Warn("cron transcript: rejecting non-UTF8 WorkDir", "job_id", jobID, "run_id", runID)
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}
	// IsLogInjectionRune covers C1 / bidi / LS-PS but intentionally NOT C0
	// controls (see osutil/loginject.go), so add the C0+DEL band explicitly:
	// an embedded tab / NUL / DEL in WorkDir would otherwise reach the
	// EvalSymlinks below with a malformed slug.
	for _, r := range run.WorkDir {
		if r < 0x20 || r == 0x7f || osutil.IsLogInjectionRune(r) {
			slog.Warn("cron transcript: rejecting WorkDir with control rune", "job_id", jobID, "run_id", runID)
			resp.Fallback = "missing"
			httputil.WriteJSON(w, resp)
			return
		}
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
			httputil.WriteJSON(w, resp)
			return
		}
		slog.Warn("cron transcript: evalsymlinks failed", "path", jsonlPath, "err", err)
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}
	// Both the resolved JSONL path AND the projects root must be canonicalised
	// before the prefix check: macOS maps /var→/private/var and symlinked
	// claudeDir components (Docker bind-mounts) drift under EvalSymlinks, so an
	// asymmetric resolve would reject every legitimate JSONL on those hosts.
	allowedRoot := filepath.Join(h.claudeDir, "projects")
	resolvedRoot, rrErr := filepath.EvalSymlinks(allowedRoot)
	if rrErr != nil {
		// Only fall back to the raw root on "dir not yet materialised". Any
		// other EvalSymlinks failure (permission denied, broken chain, IO
		// error) means we cannot trust the raw root for the prefix check — an
		// attacker-controlled symlink target could pass a lexical HasPrefix.
		// Same "missing" downgrade; the slog.Warn is the operator signal.
		if !errors.Is(rrErr, fs.ErrNotExist) {
			slog.Warn("cron transcript: allowedRoot evalsymlinks failed",
				"root", allowedRoot, "err", rrErr)
			resp.Fallback = "missing"
			httputil.WriteJSON(w, resp)
			return
		}
		resolvedRoot = allowedRoot
	}
	// Containment honours filesystem semantics, not path-string identity:
	// osutil.PathContainedInRoot does the byte-wise prefix check, then falls
	// back to an os.SameFile ancestor walk for case-insensitive filesystems
	// (macOS APFS, NTFS) where EvalSymlinks preserves user-typed case. Both
	// args are EvalSymlinks-resolved above (the helper's input contract).
	if !osutil.PathContainedInRoot(resolved, resolvedRoot) {
		slog.Warn("cron transcript: path escape attempt", "raw", jsonlPath, "resolved", resolved, "claudeDir", h.claudeDir, "allowedRoot", resolvedRoot)
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}

	// Lstat rejects non-regular files (FIFO, device, dir); then open with
	// O_NOFOLLOW + Fstat for TOCTOU defence — a symlink swap between Lstat and
	// Open would otherwise let a plain os.Open stream bytes from outside the
	// projects subtree under the original path's authorisation (#918). The
	// post-open SameFile check catches the residual same-name inode swap.
	li, err := os.Lstat(resolved)
	if err != nil {
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}
	if !li.Mode().IsRegular() {
		slog.Warn("cron transcript: non-regular file rejected", "path", resolved, "mode", li.Mode())
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}

	// dashproject.OpenWorkspaceFile passes O_NOFOLLOW on unix; a final-component
	// symlink swap therefore fails atomically at the kernel boundary
	// with ELOOP. Collapse ELOOP and any other open failure to the same
	// "missing" downgrade so attacker probing cannot distinguish a real
	// missing JSONL from a swap-then-blocked attempt.
	f, err := dashproject.OpenWorkspaceFile(resolved)
	if err != nil {
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}
	// TOCTOU inode recheck (#918): Mode().IsRegular() on the open fd catches a
	// swap-to-dir/FIFO but NOT a swap to another regular file outside the
	// projects subtree. os.SameFile compares device + inode, so a match
	// guarantees the descriptor is the exact inode Lstat validated under the
	// path-escape guard. Mismatch ⇒ swap raced; downgrade to "missing".
	if !os.SameFile(li, fi) {
		slog.Warn("cron transcript: inode swap detected post-open", "path", resolved)
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
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
		// Running run: include everything up to "now" plus slack for clock skew
		// between the cron wall-clock and the CLI JSONL writer.
		endedMS = time.Now().UnixMilli() + transcriptRunningSlackMS
	}

	tokens := transcriptTokens{}
	toolCalls := 0

	// LimitReader caps total bytes; bufio.Scanner's 256 KB buffer caps a single
	// line. Keep the concrete *io.LimitedReader so the post-scan check reads N
	// directly — f.Seek would be wrong because the scanner's read-ahead advances
	// the file offset past the logical budget even if only one line was consumed.
	// 显式 int64 cast 防止 maxTranscriptBytes 类型变更后静默截断（当前已是 int64）。
	lr := &io.LimitedReader{R: f, N: int64(maxTranscriptBytes)}
	scanner := bufio.NewScanner(lr)
	scanner.Buffer(make([]byte, 0, 64*1024), maxTranscriptLineBytes)

	turns := make([]transcriptTurn, 0, 32)
	truncated := false
	// truncateReason discriminates the Truncated cause for forensics (#1049).
	// First reason sticks so the field is deterministic when several caps fire.
	truncateReason := ""
	setTruncated := func(reason string) {
		truncated = true
		if truncateReason == "" {
			truncateReason = reason
		}
	}
	parsedAny := false

	for scanner.Scan() {
		if len(turns) >= maxTranscriptTurns {
			setTruncated("size_cap")
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
		// Time-window filter applies only to dated events. For fresh=false the
		// JSONL is shared across cron runs and we have no per-event run-id, so
		// timestamp-less events ("queue-operation", untimestamped attachments)
		// are dropped rather than leaked into an adjacent run's transcript
		// (#1046). fresh=true runs own the JSONL, so they pass through there.
		ts := parseISO8601MS(ev.Timestamp)
		if ts > 0 {
			// fresh=false shares the JSONL with adjacent runs, so a boundary
			// event (run N ended at T, run N+1 started at T) must have a single
			// owner: use the half-open [startedMS, endedMS) so it belongs to the
			// LATER run only (#642). fresh=true owns the file exclusively, so
			// the inclusive interval is safe there.
			if run.Fresh {
				if ts < startedMS || ts > endedMS {
					continue
				}
			} else {
				// half-open: ts == endedMS_N falls to run N+1.
				if ts < startedMS || ts >= endedMS {
					continue
				}
			}
		} else if !run.Fresh {
			// Shared JSONL + no timestamp ⇒ cannot attribute to this
			// run; skip rather than leak adjacent-run state.
			continue
		} else if ev.Timestamp != "" {
			// ts==0 with a non-empty source string means parseISO8601MS rejected
			// it: disk corruption or a hand-written / hostile JSONL entry that
			// could surface across every run's drawer. Drop it, matching the
			// fresh=false skip policy (#1097). Empty ev.Timestamp (legitimate
			// CLI shapes like "queue-operation") still flows through on fresh=true.
			continue
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
				setTruncated("size_cap")
				break
			}
			turns = append(turns, t)
		}
	}
	if err := scanner.Err(); err != nil {
		// Don't 5xx — the parsed prefix is still useful. Discriminate ErrTooLong
		// (malformed JSONL) from genuine IO errors (sick disk) for forensics (#1049).
		if errors.Is(err, bufio.ErrTooLong) {
			// Basename only — the full path leaks operator home / session UUID
			// to log aggregators (the path-escape warn above keeps it: security event).
			slog.Warn("cron transcript: line too long (returning partial)", "file", filepath.Base(resolved), "err", err)
			setTruncated("line_too_long")
		} else {
			slog.Warn("cron transcript: scan io error (returning partial)", "file", filepath.Base(resolved), "err", err)
			setTruncated("scan_io_error")
		}
	}

	// lr.N <= 0 means the LimitedReader has no budget left — the scan consumed
	// maxTranscriptBytes without seeing EOF — so mark truncated. Read lr.N
	// rather than f.Seek: bufio's 256 KB read-ahead can advance the file offset
	// past the cap even on a small file. lr.N does NOT track bytes still
	// queued in the scanner buffer; the reader simply refuses to top it up.
	if lr.N <= 0 {
		setTruncated("size_cap")
	}

	tokens.Total = tokens.Input + tokens.Output
	resp.Turns = turns
	resp.NextIndex = len(turns)
	resp.Truncated = truncated
	resp.TruncateReason = truncateReason
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

	httputil.WriteJSON(w, resp)
}

// parseRunPathParams extracts run_id (path) + job_id (query) and
// validates both. Centralised so HandleRunDetail and HandleRunTranscript
// share the exact same gate.
func parseRunPathParams(w http.ResponseWriter, r *http.Request) (runID, jobID string, ok bool) {
	runID = r.PathValue("run_id")
	if runID == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "run_id is required"})
		return "", "", false
	}
	if len(runID) > runIDLenLimit {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "run_id too long"})
		return "", "", false
	}
	if !cronpkg.IsValidID(runID) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "run_id must be lowercase hex"})
		return "", "", false
	}
	jobID = r.URL.Query().Get("job_id")
	if jobID == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "job_id is required"})
		return "", "", false
	}
	if len(jobID) > maxCronIDLenDashboard {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "job_id too long"})
		return "", "", false
	}
	if !cronpkg.IsValidID(jobID) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "job_id must be lowercase hex"})
		return "", "", false
	}
	return runID, jobID, true
}

// sanitizeWireText drops bidi / C1 / LS-PS runes (the IsLogInjectionRune
// class) AND C0 control bytes (except \t / \n / \r) before transcript turn
// fields reach the JSON wire, then redacts secrets. Preserves \t / \n / \r so
// multi-line tool_result rendering survives — SanitizeForLog would map those
// to '_' and destroy formatting in the dashboard's <pre> sink.
//
// Defence-in-depth: bidi overrides in a JSONL file could otherwise corrupt
// visual ordering despite esc()-then-<pre>, and 0x1B ESC would trigger ANSI
// interpretation when operators paste transcript JSON into a terminal (#1331).
func sanitizeWireText(s string) string {
	if s == "" {
		return s
	}
	// Fast path: drop nothing if string is pure ASCII printable (with the
	// three preserved whitespace runes). Any C0 control byte (< 0x20) other
	// than \t/\n/\r forces the slow path even on pure ASCII; bidi / C1
	// codepoints encode with leading byte ≥ 0x80 in UTF-8.
	dirty := false
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 0x80 {
			dirty = true
			break
		}
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			dirty = true
			break
		}
	}
	if !dirty {
		return textutil.RedactSecrets(s)
	}
	cleaned := strings.Map(func(r rune) rune {
		// Drop C0 control runes (incl. 0x1B ESC) except \t / \n / \r.
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		if osutil.IsLogInjectionRune(r) {
			return -1 // drop
		}
		return r
	}, s)
	return textutil.RedactSecrets(cleaned)
}

// flattenJSONLEvent decodes one JSONL line into 0..N transcript turns.
// Returns (turns, token deltas, tool-call delta, parsedAny); parsedAny is
// true when the event maps to at least one recognised turn shape — the
// caller uses it to decide whether to set fallback:"raw". Per-type helpers
// own their own (decode → walk → emit) sub-flow.
func flattenJSONLEvent(ev *claudeJSONLEvent, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	switch ev.Type {
	case "user":
		return flattenUserEvent(ev, ts, nextIdx)
	case "assistant":
		return flattenAssistantEvent(ev, ts, nextIdx)
	case "system":
		return flattenSystemEvent(ev, ts, nextIdx)
	}
	return nil, transcriptTokens{}, 0, false
}

// flattenUserEvent emits a "user" text turn (when content carries one) plus
// zero or more "tool_result" turns (when the user message wraps a
// content-block array — how Claude carries tool_result payloads back into the
// conversation). Two-pass: count tool_result blocks first, pre-size out
// exactly, and skip the allocation on lines that contribute no turns.
func flattenUserEvent(ev *claudeJSONLEvent, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	tok := transcriptTokens{}

	var msg claudeMessage
	if err := json.Unmarshal(ev.Message, &msg); err != nil {
		return nil, tok, 0, false
	}
	text, blocks := decodeStringOrBlocks(msg.Content)
	hasText := text != ""
	toolResultCount := 0
	for i := range blocks {
		if blocks[i].Type == "tool_result" {
			toolResultCount++
		}
	}
	totalTurns := toolResultCount
	if hasText {
		totalTurns++
	}
	if totalTurns == 0 {
		return nil, tok, 0, false
	}
	out := make([]transcriptTurn, 0, totalTurns)
	parsed := false
	if hasText {
		out = append(out, transcriptTurn{
			Index: nextIdx + len(out),
			Kind:  "user",
			TS:    ts,
			Text:  sanitizeWireText(truncateRunes(text, maxAssistantTextBytes)),
		})
		parsed = true
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		parsed = true
		outStr := toolResultText(b.Content)
		// ANSI escapes are rare in agent tool_result text; skip the regex
		// (NFA traversal of every byte) when the ESC byte 0x1b is absent,
		// which is the common case.
		if strings.IndexByte(outStr, 0x1b) >= 0 {
			outStr = ansiEscRe.ReplaceAllString(outStr, "")
		}
		outStr = sanitizeWireText(truncateRunes(outStr, maxToolOutputBytes))
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
	return out, tok, 0, parsed
}

// flattenAssistantEvent emits a single aggregated "assistant" text turn
// (multiple text blocks in one message are merged with blank-line separators
// because they split awkwardly as separate timeline entries) followed by
// per-tool_use turns. Returns the token usage delta from msg.Usage. Two-pass:
// aggregate text + count tool_use blocks, then emit at final indices — no
// prepend, no reindex, O(1) slice allocation.
func flattenAssistantEvent(ev *claudeJSONLEvent, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	tok := transcriptTokens{}
	toolCalls := 0

	var msg claudeMessage
	if err := json.Unmarshal(ev.Message, &msg); err != nil {
		return nil, tok, 0, false
	}
	_, blocks := decodeStringOrBlocks(msg.Content)
	if msg.Usage != nil {
		tok.Input = msg.Usage.InputTokens
		tok.Output = msg.Usage.OutputTokens
	}
	// First pass: aggregate text blocks + count tool_use blocks so the output
	// slice can be pre-sized exactly.
	var textBuf strings.Builder
	toolUseCount := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if textBuf.Len() > 0 {
				textBuf.WriteString("\n\n")
			}
			textBuf.WriteString(b.Text)
		case "tool_use":
			toolUseCount++
		}
	}
	hasText := textBuf.Len() > 0
	totalTurns := toolUseCount
	if hasText {
		totalTurns++
	}
	if totalTurns == 0 {
		return nil, tok, 0, false
	}
	out := make([]transcriptTurn, 0, totalTurns)
	parsed := false
	if hasText {
		out = append(out, transcriptTurn{
			Index:  nextIdx,
			Kind:   "assistant",
			TS:     ts,
			Text:   sanitizeWireText(truncateRunes(textBuf.String(), maxAssistantTextBytes)),
			Tokens: tok.Output,
		})
		parsed = true
	}
	// Second pass: emit tool_use turns in source order at indices that
	// follow the (optional) assistant turn. No reindex needed — indices
	// land in their final positions on first write.
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		toolCalls++
		summary := sanitizeWireText(summariseToolInput(b.Name, b.Input))
		// Cap the raw Input JSON we surface; summary was built from the original
		// bytes so the one-line label survives even when Input is replaced with
		// the [truncated] placeholder.
		input := b.Input
		if len(input) > maxToolInputBytes {
			input = truncatedToolInputPlaceholder
		}
		// json.RawMessage's `omitempty` only checks len==0, so a literal `null`
		// would survive as `"input": null` and confuse the dashboard's
		// "has tool input?" presence check. Normalise to a zero-length
		// RawMessage so omitempty trips (#822).
		if isJSONNull(input) {
			input = nil
		}
		// Redact secrets so credentials a cron job read into a tool call don't
		// leak verbatim (#1914).
		input = redactToolInput(input)
		out = append(out, transcriptTurn{
			Index:     nextIdx + len(out),
			Kind:      "tool_use",
			TS:        ts,
			Tool:      b.Name,
			ToolUseID: b.ID,
			Summary:   summary,
			Input:     input,
		})
		parsed = true
	}
	return out, tok, toolCalls, parsed
}

// flattenSystemEvent surfaces system error events (claude CLI lifecycle
// init / error). init events are dropped because they don't add
// timeline value; only `subtype == "error"` becomes an "error" turn.
// Unmarshal failures return early (consistent with sibling flatten helpers).
// The out slice is allocated lazily — only when an error turn is emitted.
func flattenSystemEvent(ev *claudeJSONLEvent, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	tok := transcriptTokens{}

	var sys struct {
		Subtype string `json:"subtype"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(ev.Message, &sys); err != nil {
		slog.Debug("cron transcript: system event unmarshal failed; skipping",
			"err", err)
		return nil, tok, 0, false
	}
	if sys.Subtype != "error" || sys.Message == "" {
		return nil, tok, 0, false
	}
	out := []transcriptTurn{{
		Index: nextIdx,
		Kind:  "error",
		TS:    ts,
		Text:  sanitizeWireText(truncateRunes(sys.Message, maxAssistantTextBytes)),
	}}
	return out, tok, 0, true
}

// toolInputProbe is the partial schema summariseToolInput decodes into to
// pick a one-liner label (Bash → command, Read/Write/Edit → file_path, …).
// A typed struct avoids the reflection + map cost of `map[string]any` per
// transcript line; unrecognised keys are skipped by encoding/json (#1010).
type toolInputProbe struct {
	Command  string `json:"command,omitempty"`
	FilePath string `json:"file_path,omitempty"`
	Path     string `json:"path,omitempty"`
	URL      string `json:"url,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
	Query    string `json:"query,omitempty"`
}

// summariseToolInput builds a one-line label for the tool_use card header.
// Best-effort: Bash → command, Read/Write/Edit → file_path, otherwise a
// JSON-trimmed dump of the input. Inputs above summariseInputCap are refused
// before json.Unmarshal so a deeply-nested blob cannot drive the parser just
// to populate a 200-byte label (#645).
func summariseToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	if len(input) > summariseInputCap {
		// Oversize input is opaque: the wire payload already carries the
		// [truncated] placeholder and the dashboard handles a missing summary.
		return ""
	}
	var probe toolInputProbe
	if err := json.Unmarshal(input, &probe); err != nil {
		return ""
	}
	// Priority order so callers see deterministic output when a tool
	// populates multiple fields.
	candidates := [...]string{
		probe.Command, probe.FilePath, probe.Path,
		probe.URL, probe.Pattern, probe.Query,
	}
	for _, s := range candidates {
		if s != "" {
			return textutil.RedactSecrets(osutil.SanitizeForLog(s, 200))
		}
	}
	// Fallback: reuse the original bytes (json.Unmarshal does not mutate its
	// source), no need to Marshal again.
	return textutil.RedactSecrets(osutil.SanitizeForLog(string(input), 200))
}

// isJSONNull reports whether b is the JSON `null` literal (with optional
// surrounding ASCII whitespace per RFC 8259). Used to suppress an upstream
// `"input": null` so the wire response honours the RawMessage `omitempty`
// contract (#822).
func isJSONNull(b json.RawMessage) bool {
	// RFC 8259 permits insignificant whitespace (sp/tab/lf/cr) outside
	// structural tokens; trim conservatively before the byte compare.
	for len(b) > 0 {
		switch b[0] {
		case ' ', '\t', '\n', '\r':
			b = b[1:]
		default:
			goto trail
		}
	}
trail:
	for len(b) > 0 {
		switch b[len(b)-1] {
		case ' ', '\t', '\n', '\r':
			b = b[:len(b)-1]
		default:
			goto compare
		}
	}
compare:
	return len(b) == 4 && b[0] == 'n' && b[1] == 'u' && b[2] == 'l' && b[3] == 'l'
}

// redactToolInput strips well-known secret tokens (API keys, passwords, …)
// out of the raw tool_use.Input JSON before it reaches the wire; the Summary
// is already redacted via sanitizeWireText but the full Input surfaced
// verbatim (#1914).
//
// The redaction MUST leave valid JSON behind: an `input` json.Encoder refuses
// to emit fails the whole response (WriteJSON → 500). RedactSecrets' `KEY=value`
// masking can swallow a closing `"}` inside a JSON string, so redactRawJSON
// falls back to per-string-value redaction whenever the text-level result is
// malformed. Clean input is returned unchanged (aliased).
func redactToolInput(in json.RawMessage) json.RawMessage {
	if len(in) == 0 {
		return in
	}
	out := redactRawJSON(string(in), textutil.RedactSecrets)
	if string(out) == string(in) {
		return in
	}
	return out
}

// parseISO8601MS converts an RFC 3339 / ISO 8601 timestamp into unix ms.
// Returns 0 when the input is empty or unparseable so callers can use it as a
// "skip filter" sentinel. time.RFC3339Nano is a strict superset of RFC3339
// (the fractional part is optional), so no second layout is needed.
//
// The Claude CLI exclusively emits "YYYY-MM-DDTHH:MM:SS[.fff…]Z", which the
// byte-level fast path parses in ~30ns vs ~300ns for time.Parse — compounding
// across 500-line transcripts under bulk polling (#1012). Anything
// non-canonical (offsets, exotic layouts) falls back to time.Parse, so
// results are bit-identical to the slow path.
func parseISO8601MS(s string) int64 {
	if s == "" {
		return 0
	}
	if ms, ok := parseISO8601MSFast(s); ok {
		return ms
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// parseISO8601MSFast hand-parses the canonical UTC RFC 3339 shape the Claude
// CLI emits and returns (unixMillis, true) on success:
//
//	YYYY-MM-DDTHH:MM:SS(.fffffffff)?Z
//
// Any deviation (offset other than 'Z', missing field, non-digit, lowercase
// 't'/'z') returns (0, false) and the caller falls back to time.Parse. Each
// field is range-checked before time.Date because time.Date *normalises*
// out-of-range values (month 13 → next January) whereas time.Parse rejects
// them; seconds cap at 59 since time.Parse does not honour leap seconds.
func parseISO8601MSFast(s string) (int64, bool) {
	// Minimum canonical length is "YYYY-MM-DDTHH:MM:SSZ" = 20 bytes.
	if len(s) < 20 {
		return 0, false
	}
	// Fixed-position separator check before any digit work.
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' ||
		s[13] != ':' || s[16] != ':' {
		return 0, false
	}
	year, ok := parseDigits(s[0:4])
	if !ok {
		return 0, false
	}
	month, ok := parseDigits(s[5:7])
	if !ok {
		return 0, false
	}
	day, ok := parseDigits(s[8:10])
	if !ok {
		return 0, false
	}
	hour, ok := parseDigits(s[11:13])
	if !ok {
		return 0, false
	}
	minute, ok := parseDigits(s[14:16])
	if !ok {
		return 0, false
	}
	second, ok := parseDigits(s[17:19])
	if !ok {
		return 0, false
	}
	// Range-check so we reject what time.Parse rejects instead of letting
	// time.Date normalise; day is validated per-month (leap-year aware).
	if month < 1 || month > 12 ||
		day < 1 || day > daysInMonth(year, month) ||
		hour > 23 ||
		minute > 59 ||
		second > 59 {
		return 0, false
	}
	// After SS we expect either:
	//   - "Z"          (no fractional seconds)
	//   - ".<digits>Z" (1..9 fractional digits, RFC3339Nano)
	nanos := 0
	rest := s[19:]
	if rest[0] == '.' {
		// Find the trailing 'Z' and require 1..9 fractional digits.
		if len(rest) < 3 { // need at least ".dZ"
			return 0, false
		}
		// Locate Z and verify all interior chars are digits.
		fracEnd := -1
		for i := 1; i < len(rest); i++ {
			c := rest[i]
			if c == 'Z' {
				fracEnd = i
				break
			}
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		if fracEnd < 2 || fracEnd != len(rest)-1 {
			return 0, false
		}
		fracDigits := rest[1:fracEnd]
		if len(fracDigits) > 9 {
			return 0, false
		}
		// Convert fractional seconds into nanoseconds. Pad on the right
		// with implicit zeros so ".5" → 500000000ns, ".123" → 123000000ns.
		nanos, ok = parseDigits(fracDigits)
		if !ok {
			return 0, false
		}
		for i := len(fracDigits); i < 9; i++ {
			nanos *= 10
		}
	} else if rest == "Z" {
		// canonical SS Z, no fractional seconds.
	} else {
		return 0, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, nanos, time.UTC)
	return t.UnixMilli(), true
}

// daysInMonth returns the number of days in the given (year, month). month
// is assumed to already be in 1..12. February is leap-year aware so the
// fast path's day range-check matches time.Parse's calendar validation.
func daysInMonth(year, month int) int {
	switch month {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	default: // February
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	}
}

// parseDigits parses a fixed-length all-ASCII-digits string as a non-negative
// int. Returns (n, true) on success, (0, false) on any non-digit. Beats
// strconv.Atoi by skipping the leading-sign / leading-zero handling.
func parseDigits(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// truncateRunes caps a string to maxBytes by rune boundary, appending "…"
// (3 bytes) when truncation happened. Cutting by rune rather than byte keeps
// multi-byte UTF-8 sequences intact (a split would render as U+FFFD). The
// walk stops when adding the next rune would push the final length (after
// the "…" suffix) past maxBytes, so the result is never over the cap.
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…" // 3 bytes UTF-8.
	// Honour the cap even when maxBytes is too small to fit the ellipsis —
	// without this the function would return the bare ellipsis (3 bytes) on
	// any maxBytes < 3 input, violating the "caps to maxBytes" contract.
	if maxBytes < len(ellipsis) {
		return ""
	}
	budget := maxBytes - len(ellipsis)
	// cut tracks the byte offset where we may safely cut: the end of the
	// last rune we have committed. Iterating with range gives us the start
	// byte index of each rune; we commit a rune when its end position fits
	// the budget.
	cut := 0
	for i, r := range s {
		size := utf8.RuneLen(r)
		if size < 0 {
			size = len(string(utf8.RuneError))
		}
		if i+size > budget {
			break
		}
		cut = i + size
	}
	return s[:cut] + ellipsis
}
