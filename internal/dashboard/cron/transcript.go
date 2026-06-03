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

	// maxToolInputBytes caps the raw tool_use.Input JSON we surface to the
	// dashboard (R234-SEC-8). Without this, a transcript containing 500
	// turns × 256KB lines of tool_use.Input would push ~128MB of bytes into
	// the response per request — the request handler is auth'd but that
	// is still a trivial dashboard-side memory amplifier. Bash command
	// payloads / Read tool args / Edit diffs all fit comfortably in 64KB;
	// pathological cases (a tool that streams binary as a single-call
	// argument) get a "[truncated]" placeholder so the timeline still
	// renders the call but no longer ships the full payload.
	maxToolInputBytes = 64 * 1024

	// summariseInputCap is the upper byte limit for a tool_use.Input we
	// will feed to json.Unmarshal in summariseToolInput. The probe only
	// surfaces six short string fields (command, file_path, …) and the
	// fallback is truncated to 200 bytes, so a useful one-line label never
	// needs more than a few KB of input. Capping well below the wire
	// payload limit (maxToolInputBytes = 64 KB) shrinks the amount of
	// attacker-influenced JSON handed to json.Unmarshal: at
	// maxTranscriptTurns=500 × transcriptSem(8) the worst-case unmarshal
	// fan-out drops from 500×128 KB ≈ 64 MB to 500×16 KB ≈ 8 MB per
	// request. Inputs above this cap are rejected before json.Unmarshal so
	// a hostile transcript line cannot drive the parser through a large
	// deeply-nested blob just to populate a 200-byte label.
	// R242-SEC-13 (#645); R20260602141221-SEC-9 (#1584) lowered 128 KB→16 KB.
	summariseInputCap = 16 * 1024

	// transcriptRunningSlackMS is the slack added to "now" when computing
	// the upper bound of the time window for a still-running cron run.
	// fresh=false runs share a JSONL across many invocations, so we filter
	// turns by [run.StartedAt, endedMS]. While the run is still going we
	// have no run.EndedAt, so we use time.Now()+slack to absorb clock skew
	// between the cron wall-clock and the JSONL writer (CLI subprocess) —
	// neither is NTP-synced in test fixtures, and a turn timestamp slightly
	// ahead of "now" should still appear in the live transcript view.
	transcriptRunningSlackMS int64 = 5_000
)

// The concurrency gate for HandleRunTranscript lives on the Handlers
// struct as transcriptSem (wired by server/build_handlers.go with
// cronTranscriptSemCap=8). See the acquire site in HandleRunTranscript
// and the field godoc in handlers.go for the R243-SEC-12 (#798)
// rationale — each in-flight transcript holds a 256 KB scanner buffer
// plus an 8 MB LimitReader budget, so a process-wide ceiling bounds the
// memory amplifier under multi-operator load.

// truncatedToolInputPlaceholder is the JSON value substituted for
// tool_use.Input fields that exceed maxToolInputBytes. Pre-encoded so the
// hot path never re-marshals; must be a valid JSON value (a string
// literal here) so the wire shape stays consistent for dashboard JS.
var truncatedToolInputPlaceholder = json.RawMessage(`"[truncated]"`)

// ansiEscRe matches the most common ANSI CSI sequences (color, cursor
// motion) AND OSC sequences (operating-system commands such as the
// hyperlink escape `\x1b]8;;url\x1b\\` / BEL-terminated `\x1b]8;;url\x07`).
// We strip these from tool output before serialising so the rendered
// <pre> doesn't show garbled bytes. Defensive: the dashboard uses
// esc()-then-<pre> so the bytes wouldn't be interpreted as HTML either
// way, but they'd render as literal escape codes which hurt readability
// for a debugging-focused view.
//
// R243-SEC-6 (#788): the regex previously covered only CSI (`\x1b[`),
// leaving OSC hyperlinks (used by `gh`, modern `ls --hyperlink`, and
// language-server output) intact. Extend the alternation so both
// terminators (BEL `\x07` and ST `\x1b\\`) are scrubbed together with
// CSI. The two halves run as one Go RE2 alternation so a single regex
// pass covers both classes — no extra hot-path allocation.
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
	// TruncateReason discriminates why Truncated is true so forensics can
	// distinguish a legitimate size-cap hit from a disk read error or an
	// over-long JSONL line. Only populated when Truncated is true.
	// R240-SEC-8 / #1049.
	//
	//   ""               — Truncated=false (normal path) or legacy producers
	//   "size_cap"       — hit maxTranscriptBytes / maxTranscriptTurns
	//   "line_too_long"  — bufio.ErrTooLong (one line exceeded
	//                      maxTranscriptLineBytes)
	//   "scan_io_error"  — Scanner.Err returned a non-ErrTooLong error
	//                      (disk read failure, truncated file mid-syscall)
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

// transcriptTurn is a single rendered timeline entry. Only fields
// relevant to its kind are populated; absent fields stay at zero values
// (omitted via json:"omitempty"). Index reflects this turn's position in
// the *response*, not the original JSONL line — the dashboard uses it
// for stable React-style keys when diffing live updates.
//
// CLIENT-SIDE CONTRACT (R249-SEC-8 / #921): the Input field is forwarded
// as raw JSON bytes from the CLI's tool_use payload. httputil.WriteJSON disables
// SetEscapeHTML (see dashboard.go httputil.WriteJSON R71-SEC-L1 godoc), so any
// `<`, `>`, `&` literals embedded in the original tool input survive
// onto the wire verbatim. Today's dashboard.js renders Input via
// JSON.stringify + esc() before assembling DOM, which is safe; a future
// debug viewer that injects Input directly via innerHTML — without
// DOMPurify — would immediately become a stored-XSS sink because tool
// input is attacker-influenced (a malicious project file can steer the
// CLI's tool calls). When introducing a new consumer of this field,
// mirror the existing Text / Output sanitizeWireText pattern or route
// the bytes through DOMPurify before any innerHTML assignment. The
// upstream maxToolInputBytes cap + truncatedToolInputPlaceholder
// substitution keep Input bounded but do not normalise its byte set.
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
	// R250-SEC-7 (#1096): use the dedicated transcriptLimiter rather
	// than the shared runsLimiter. The transcript path fans out far
	// more I/O than the runs-list endpoint (EvalSymlinks ×2 + 8 MB
	// LimitReader + 256 KB scanner buffer + per-line json.Unmarshal +
	// flattenJSONLEvent) so sharing one bucket lets either endpoint
	// starve the other under load. Fall back to runsLimiter for older
	// hand-rolled Handlers fixtures (newCronHandlersForTest) that
	// haven't been wired with a transcriptLimiter.
	limiter := h.transcriptLimiter
	if limiter == nil {
		limiter = h.runsLimiter
	}
	if limiter != nil && !limiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron transcript rate limit exceeded"})
		return
	}
	// R243-SEC-12 (#798): cap concurrent in-flight transcript reads.
	// Each running scan holds 256 KB of bufio.Scanner buffer plus an
	// 8 MB LimitReader budget; without this gate, N distinct
	// authenticated operators can each saturate their per-IP
	// runsLimiter and collectively park N×8 MB of file-mapped pages
	// + N×256 KB of scanner buffers. The non-blocking acquire keeps
	// the failure mode "503 immediately" instead of "slow-loris
	// holds a goroutine open until the request context expires" —
	// matches the transcribeSemCap pattern. Acquired BEFORE the
	// scheduler-nil check so the gate is testable in isolation
	// (handlers built without a scheduler can still exercise the
	// busy fast-fail path); a 503 here also short-circuits the
	// scheduler lookup, which is mildly cheaper than the reverse
	// ordering. Nil-guarded so older hand-rolled Handlers
	// fixtures (newCronHandlersForTest) skip the gate.
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

	run, err := h.scheduler.GetRun(jobID, runID)
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
	// R236-SEC-13: defence in depth before ClaudeProjectSlug encodes
	// WorkDir into a filesystem path component. ClaudeProjectSlug only
	// replaces '/' with '-' and strips the leading separator — it does
	// NOT scrub C0/C1/bidi/zero-width or invalid UTF-8. A persisted run
	// from a hand-edited disk file (or an older naozhi version that
	// allowed weaker WorkDir validation) could carry a control rune
	// that tunnels through the slug into the projects/ directory name
	// and lets the EvalSymlinks below land on an unintended path.
	// Reject before constructing jsonlPath so the strict check below is
	// not asked to defend against a malformed input.
	if !utf8.ValidString(run.WorkDir) {
		slog.Warn("cron transcript: rejecting non-UTF8 WorkDir", "job_id", jobID, "run_id", runID)
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}
	// R242-SEC-14: IsLogInjectionRune covers C1 / bidi / LS-PS but
	// intentionally NOT C0 controls (see osutil/loginject.go godoc —
	// "Callers that also need to reject C0 controls (< 0x20) should
	// gate on `r < 0x20 || r == 0x7f` separately"). A persisted run
	// with an embedded tab / NUL / DEL in WorkDir (older naozhi
	// versions or hand-edited disk files) would otherwise slip
	// through this guard and reach the EvalSymlinks below with a
	// malformed slug. Add the C0+DEL band explicitly so the strict
	// check downstream is not asked to defend against shell control
	// characters.
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
	// Both the resolved JSONL path AND the claudeDir+projects root must be
	// canonicalised before the prefix check. macOS canonicalises /var to
	// /private/var, and any host where claudeDir contains a symlinked
	// component (Docker bind-mounts, AMI-customised layouts) similarly
	// drifts under EvalSymlinks. Without the symmetric resolve the prefix
	// check rejects every legitimate JSONL on those hosts.
	allowedRoot := filepath.Join(h.claudeDir, "projects")
	resolvedRoot, rrErr := filepath.EvalSymlinks(allowedRoot)
	if rrErr != nil {
		// R240-SEC-3: only fall back on the "fresh install / dir not yet
		// materialised" case. Any *other* EvalSymlinks failure (permission
		// denied, broken symlink chain, IO error) means we cannot trust the
		// raw root for the prefix comparison below — if allowedRoot is itself
		// a symlink we don't know where it points, so an attacker-controlled
		// symlink target could pass the lexical HasPrefix check against the
		// raw path. Return the same "missing" downgrade the dashboard already
		// renders for absent transcripts; the operator-visible signal is the
		// slog.Warn line below.
		if !errors.Is(rrErr, fs.ErrNotExist) {
			slog.Warn("cron transcript: allowedRoot evalsymlinks failed",
				"root", allowedRoot, "err", rrErr)
			resp.Fallback = "missing"
			httputil.WriteJSON(w, resp)
			return
		}
		resolvedRoot = allowedRoot
	}
	// R236-SEC-05: align with the validateWorkspace / workDirUnderRoot
	// pattern (`resolved != root && !HasPrefix(resolved, root+sep)`) so a
	// future refactor can grep for one shape across server/cron. The old
	// "double-append separator" form was correct but easy to misread as
	// "if I forget the trailing sep on one side, the check is now wrong",
	// which is the failure mode the unified pattern eliminates.
	//
	// R238-SEC-6: HasPrefix is byte-wise case-sensitive. On macOS APFS /
	// HFS+ (default case-insensitive) and Windows NTFS, EvalSymlinks may
	// preserve the user-typed case for path components that the kernel
	// otherwise treats as equivalent — e.g. resolved="/Users/alice/.claude/projects/..."
	// vs resolvedRoot="/Users/Alice/.claude/projects" would falsely fail
	// the prefix check and downgrade every legitimate run to "missing".
	// Fall back to a SameFile-walk on the resolved path's ancestors when
	// the byte-wise check fails so the gate matches actual filesystem
	// containment semantics rather than path-string identity.
	if resolved != resolvedRoot &&
		!strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator)) &&
		!sameFileAncestor(resolved, resolvedRoot) {
		slog.Warn("cron transcript: path escape attempt", "raw", jsonlPath, "resolved", resolved, "claudeDir", h.claudeDir, "allowedRoot", resolvedRoot)
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}

	// Lstat to reject non-regular files (FIFO, device, dir-with-name
	// matching). Then open with O_NOFOLLOW + Fstat for TOCTOU defence:
	// the symlink-swap could happen between Lstat and Open, and a plain
	// os.Open would silently follow the swapped symlink and stream bytes
	// from outside the projects subtree under the original path's
	// authorisation. R249-SEC-4 (#918) closes that window the same way
	// handleFileGet / handleAttachment already do — see the R219-SEC-2 /
	// R249-SEC-3 prior art in project_files.go and dashboard_send.go.
	// The post-open Fstat re-check still catches a same-inode swap to a
	// non-regular file (the residual TOCTOU after O_NOFOLLOW eliminates
	// the symlink leg).
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
	// R249-SEC-4 (#918): TOCTOU inode recheck. The Lstat above resolved
	// the dirent to a regular-file inode, then a hostile symlink swap
	// (filename → /etc/shadow) could in theory race the os.Open call
	// before the file descriptor binds. Mode().IsRegular() on the open
	// fd catches a swap-to-dir/FIFO but NOT a swap to another regular
	// file outside the projects subtree. os.SameFile checks the device
	// + inode pair so a successful match guarantees the open descriptor
	// references the exact inode Lstat validated under the path-escape
	// guard above. Mismatch ⇒ swap raced; fall back to "missing" rather
	// than read potentially exfiltrated bytes.
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
		// Running run: include everything up to "now". A small slack
		// (transcriptRunningSlackMS) handles clock skew between the
		// cron wall-clock and the JSONL writer (CLI subprocess),
		// neither of which is NTP-synced in test fixtures.
		endedMS = time.Now().UnixMilli() + transcriptRunningSlackMS
	}

	tokens := transcriptTokens{}
	toolCalls := 0

	// LimitReader caps total bytes read; bufio.Scanner with a 256 KB
	// buffer caps single-line bytes. Together they enforce the
	// design's three-tier size budget without ever calling
	// os.ReadFile on the underlying file.
	//
	// io.LimitReader always returns *io.LimitedReader; we keep the
	// concrete type so the post-scan check below can read N directly
	// without type assertion. Using f.Seek to detect cap-hit would be
	// wrong: bufio.Scanner pre-fills a 256 KB buffer, so the underlying
	// file offset can advance well past the LimitReader's logical
	// budget even when the scanner only consumed the first line.
	// 显式 int64 cast 防止 maxTranscriptBytes 类型变更后静默截断（当前已是 int64）。
	lr := &io.LimitedReader{R: f, N: int64(maxTranscriptBytes)}
	scanner := bufio.NewScanner(lr)
	scanner.Buffer(make([]byte, 0, 64*1024), maxTranscriptLineBytes)

	turns := make([]transcriptTurn, 0, 32)
	truncated := false
	// truncateReason discriminates Truncated cause for forensics
	// (R240-SEC-8 / #1049). Set alongside `truncated = true`. First
	// reason sticks — we report the earliest cause to keep the
	// reason field deterministic when multiple caps trigger.
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
		// Time-window filter applies only to dated events. The CLI
		// writes "queue-operation" / "attachment" events without
		// timestamps for some shapes; we let those pass through and
		// drop later if they're not turn-worthy.
		//
		// R240-SEC-15 / #1046: BUT for fresh=false (shared JSONL across
		// many cron runs), letting timestamp-less events pass through
		// causes adjacent runs to bleed into each other's transcript —
		// a "queue-operation" or untimestamped attachment from run N+1
		// would appear in the response for run N because the
		// time-window gate is skipped. We have no per-event run-id to
		// disambiguate, so the safe-by-default rule for shared files
		// is: drop timestamp-less events entirely. The cost is a few
		// missed metadata events on the boundary; the alternative
		// (cross-run leak of attachments / queue ops with potentially
		// sensitive content) is strictly worse. Fresh=true runs own
		// the JSONL exclusively, so the existing pass-through behaviour
		// remains correct there.
		ts := parseISO8601MS(ev.Timestamp)
		if ts > 0 {
			// R242-SEC-12 (#642): for fresh=false the JSONL is shared
			// across adjacent cron runs, so an event whose timestamp
			// lands exactly on the millisecond boundary between two
			// runs (run N ended at T, run N+1 started at T) was
			// previously included in BOTH runs' transcript responses
			// because the time-window check uses `ts > endedMS` on the
			// upper side AND `ts < startedMS` on the lower side — both
			// half-open in the wrong direction, so ts==endedMS_N and
			// ts==startedMS_{N+1} both pass the gate. Without a per-run
			// UUID we cannot reliably attribute the boundary event;
			// the safe-by-default fix is the standard half-open
			// interval [startedMS, endedMS) so a boundary event is
			// claimed by the LATER run only (deterministic single
			// owner). For fresh=true the JSONL is exclusively owned by
			// this run, so the inclusive boundary is preserved (both
			// ends inclusive matches the previous behaviour and is
			// safe since no adjacent run shares the file).
			if run.Fresh {
				if ts < startedMS || ts > endedMS {
					continue
				}
			} else {
				// fresh=false: half-open [startedMS, endedMS). The
				// boundary event ts == endedMS_N is rejected here for
				// run N; it falls to run N+1 (whose startedMS_{N+1}
				// == endedMS_N is the inclusive lower bound). Single
				// owner per ts boundary, no leak.
				if ts < startedMS || ts >= endedMS {
					continue
				}
			}
		} else if !run.Fresh {
			// Shared JSONL + no timestamp ⇒ cannot attribute to this
			// run; skip rather than leak adjacent-run state.
			continue
		} else if ev.Timestamp != "" {
			// R250-SEC-8 (#1097): ts==0 but the source timestamp string
			// is non-empty means parseISO8601MS rejected the input. Even
			// for fresh=true (this run owns the JSONL exclusively) an
			// unparseable timestamp signals either disk corruption or a
			// hand-written / hostile JSONL entry that an operator with
			// workspace write access could craft to surface across every
			// run's transcript drawer. Align parse-failure with the
			// empty-timestamp skip policy already used for fresh=false:
			// drop rather than include. Empty ev.Timestamp (legitimate
			// CLI shapes like "queue-operation" without a timestamp) is
			// preserved on the fresh=true branch so existing metadata
			// continues to flow through.
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
		// Don't 5xx — the prefix we did parse is still useful.
		// R240-SEC-8 / #1049: discriminate ErrTooLong (oversize line)
		// from genuine IO errors (disk read failure, file truncated
		// mid-syscall). Forensics need this distinction — collapsing
		// both into truncated=true loses the signal that the JSONL
		// file itself was malformed vs. the disk was sick.
		if errors.Is(err, bufio.ErrTooLong) {
			// R20260602141221-SEC-13: use basename only — full path leaks
			// operator home/workspace/session UUID to log aggregators.
			// The escape-attempt log at line 461 retains full path (security event).
			slog.Warn("cron transcript: line too long (returning partial)", "file", filepath.Base(resolved), "err", err)
			setTruncated("line_too_long")
		} else {
			slog.Warn("cron transcript: scan io error (returning partial)", "file", filepath.Base(resolved), "err", err)
			setTruncated("scan_io_error")
		}
	}

	// LimitReader hit means we read maxTranscriptBytes worth without
	// seeing EOF. Mark truncated too. Read lr.N directly: bufio's
	// 256 KB read-ahead can advance the underlying *os.File offset
	// past maxTranscriptBytes even on a small file, so f.Seek would
	// false-positive truncation.
	//
	// R244-GO-P1-1: lr.N is the number of bytes the LimitedReader will
	// still hand out to its consumer (bufio.Scanner) on subsequent Read
	// calls. It is decremented by each successful Read on lr by the
	// number of bytes returned, so `lr.N <= 0` means the LimitedReader
	// has no bytes left to give bufio.Scanner — equivalently, the scan
	// loop above either consumed exactly maxTranscriptBytes or stopped
	// before that point with no remaining budget. It does NOT track
	// "logical bytes still queued in the parser" — bufio.Scanner's
	// internal 256 KB buffer may still hold partly-parsed data, but
	// the LimitedReader will refuse to top it up further.
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
		http.Error(w, "run_id is required", http.StatusBadRequest)
		return "", "", false
	}
	if len(runID) > runIDLenLimit {
		http.Error(w, "run_id too long", http.StatusBadRequest)
		return "", "", false
	}
	if !cronpkg.IsValidID(runID) {
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
	if !cronpkg.IsValidID(jobID) {
		http.Error(w, "job_id must be lowercase hex", http.StatusBadRequest)
		return "", "", false
	}
	return runID, jobID, true
}

// sanitizeWireText drops bidi / C1 / LS-PS runes (the IsLogInjectionRune
// class) AND C0 control bytes (except \t / \n / \r) before transcript turn
// fields reach the JSON wire. Preserves \t / \n / \r so multi-line
// tool_result rendering survives — calling SanitizeForLog directly would
// map those to '_' and destroy formatting in the dashboard's <pre> sink.
//
// R243-SEC-5: HandleRunDetail runs Prompt/WorkDir through the strict
// SanitizeForLog before wire-encode; the JSONL transcript path skipped
// sanitisation entirely, so a JSONL file with bidi overrides could reach
// the dashboard verbatim and corrupt visual ordering despite esc()-then-
// <pre>. Defence-in-depth.
//
// R20260527122801-SEC-7 (#1331): IsLogInjectionRune only covers C1
// (0x80..9F) + bidi + LS/PS — 0x1B ESC (and other C0 control bytes) used
// to flow through verbatim. Operators copy-pasting transcript JSON into a
// terminal viewer would then trigger ANSI escape interpretation. Drop all
// r < 0x20 except the three whitespace chars we explicitly preserve.
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
// Returns (turns, token deltas, tool-call delta, parsedAny).
//
// parsedAny is true when the event maps to at least one recognised turn
// shape — used by the caller to decide whether to set fallback:"raw".
//
// R242-CR-13: previously this function held three event-type cases inline
// at 4-5 levels of nesting (~120 lines). Per-type extraction below
// flattens the dispatch to a single switch and lets each helper own
// just its own (decode → walk → emit) sub-flow.
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

// flattenUserEvent emits a "user" text turn (when content carries one)
// plus zero or more "tool_result" turns (when the user message wraps
// a content-block array). content can be a plain string OR a
// content-block array (the latter is how Claude carries tool_result
// payloads back into the conversation).
//
// R241-PERF-7: previously this allocated `make([]transcriptTurn, 0, 2)`
// per JSONL line — on a 500-line cron transcript that's 500 grow-prone
// 2-cap headers even when the line decoded into zero or one turns. We
// now mirror flattenAssistantEvent's two-pass shape: count tool_result
// blocks first, pre-size out exactly (text? + N×tool_result), and skip
// the allocation entirely on lines that contribute no turns. Negligible
// CPU cost (one extra pass over `blocks`, which is already in-cache from
// decodeStringOrBlocks) for an O(N) → O(parsed-rows) allocation drop.
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
		outStr, _ := decodeStringOrBlocks(b.Content)
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
// (multiple text blocks in one message are merged with blank-line
// separators because they split awkwardly when shown as separate
// timeline entries) followed by per-tool_use turns. Returns the token
// usage delta from msg.Usage so callers can advance the running total.
//
// R247-PERF-2 / R247-PERF-18: the previous implementation emitted tool_use
// turns first (with provisional indices nextIdx + len(out)), then prepended
// the assistant turn via `append([]turn{a}, out...)` and re-indexed every
// element. Each call therefore allocated a fresh backing slice and reindexed
// O(N) — on a 500-row transcript the prepend dominated the parse cost. We
// now build textBuf in a first pass over `blocks`, emit the assistant turn
// first (when present) at the deterministic nextIdx, then emit tool_use
// turns at sequential indices in a second pass over the same blocks. No
// prepend, no reindex; allocations stay at O(1) for the slice header
// regardless of textBuf vs tool_use mix.
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
	// First pass: aggregate text blocks + count tool_use blocks so we can
	// pre-size the output slice exactly. Avoids the per-row `make([]T,0,2)`
	// + grow churn flagged by R247-PERF-18 across 500-row transcripts.
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
		// R234-SEC-8: cap the raw Input JSON we surface. summary is built
		// from a probe-Unmarshal of the original bytes (still bounded by
		// the per-line maxTranscriptLineBytes), so the dashboard timeline
		// keeps its one-line label even when Input itself is replaced
		// with the [truncated] placeholder.
		input := b.Input
		if len(input) > maxToolInputBytes {
			input = truncatedToolInputPlaceholder
		}
		// R243-CR-P2-4 / #822: json.RawMessage's `omitempty` only checks
		// `len(value) == 0`, so a literal `null` (4 bytes) survives as a
		// `"input": null` field on the wire — confusing the dashboard's
		// "card has tool input?" check (truthy on the field's *presence*
		// rather than its value) and wasting bytes on a value the field
		// is supposed to omit by zero-value semantics. Normalise the JSON
		// `null` literal to a zero-length RawMessage so omitempty trips
		// correctly. Whitespace-only variants (`null`, `null\n`, etc.)
		// don't appear in CLI output — RFC 8259 forbids interior
		// whitespace in JSON literals — but bytes.TrimSpace + compare
		// would handle a future formatter blip with negligible cost.
		if isJSONNull(input) {
			input = nil
		}
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

// toolInputProbe is the partial schema used by summariseToolInput to pick
// the most useful one-liner label for a tool_use card header. Only the
// six fields below are surfaced today (Bash → command, Read/Write/Edit →
// file_path, etc.); decoding into this typed struct avoids the reflection
// + map allocation cost of `map[string]any` on every transcript line.
// Unrecognised keys are silently skipped by encoding/json, so a future CLI
// adding new tool-input fields will not break parsing.
//
// R233-PERF-5 (#1010): replaces a per-line `var obj map[string]any` decode.
type toolInputProbe struct {
	Command  string `json:"command,omitempty"`
	FilePath string `json:"file_path,omitempty"`
	Path     string `json:"path,omitempty"`
	URL      string `json:"url,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
	Query    string `json:"query,omitempty"`
}

// summariseToolInput builds a one-line label for the tool_use card
// header. Best-effort: Bash → command, Read/Write/Edit → file_path,
// otherwise fall back to a JSON-trimmed dump of the input.
//
// R242-SEC-13 (#645): cap the JSON input handed to encoding/json. The
// per-line bufio.Scanner buffer (maxTranscriptLineBytes = 256 KB) already
// bounds a single transcript line, but a maximally-sized tool_use.Input
// can still drive json.Unmarshal through a deeply-nested 256 KB blob just
// to populate six string fields and a fallback that ends up truncated
// to 200 bytes anyway. Refuse anything beyond summariseInputCap up front
// so the parser never sees the amplifier shape — the probe only needs a
// few KB to find a label, so the cap sits well below the wire payload
// limit (maxToolInputBytes = 64 KB) to minimise unmarshal fan-out.
func summariseToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	if len(input) > summariseInputCap {
		// Treat oversize input as opaque: the caller will already have
		// rendered the [truncated] placeholder for the wire payload,
		// and a one-line label built from a 64 KB+ blob is not useful
		// anyway. Returning empty drops the header summary line; the
		// dashboard already handles missing summaries.
		return ""
	}
	var probe toolInputProbe
	if err := json.Unmarshal(input, &probe); err != nil {
		return ""
	}
	// Iterate in priority order (matches the previous map-based code's
	// `prefer` slice) so callers see deterministic output when a tool
	// happens to populate multiple fields.
	candidates := [...]string{
		probe.Command, probe.FilePath, probe.Path,
		probe.URL, probe.Pattern, probe.Query,
	}
	for _, s := range candidates {
		if s != "" {
			return textutil.RedactSecrets(osutil.SanitizeForLog(s, 200))
		}
	}
	// Fallback: reuse the original input bytes (json.Unmarshal does not
	// mutate its source). The probe struct only validated structure, no
	// need to Marshal again. R244-GO-P2-2.
	return textutil.RedactSecrets(osutil.SanitizeForLog(string(input), 200))
}

// isJSONNull reports whether b is the JSON `null` literal (with optional
// surrounding ASCII whitespace per RFC 8259). Used to suppress an upstream
// `"input": null` from the tool_use turn so the wire response matches the
// `omitempty` contract callers reasonably expect from a RawMessage field.
// R243-CR-P2-4 / #822.
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

// parseISO8601MS converts an RFC 3339 / ISO 8601 timestamp into unix ms.
// Returns 0 when the input is empty or unparseable so callers can use
// it as a fall-through "skip filter" sentinel.
//
// time.RFC3339Nano is a strict superset of time.RFC3339 — any timestamp
// the latter accepts is also accepted by the former — so the previous
// RFC3339 fallback was dead code and is now removed (R243-CR-P3-6).
// (Go time.Parse treats .999... fragment as optional, so RFC3339Nano layout accepts both fractional and non-fractional inputs.)
//
// R234-PERF-10 / #1012: time.Parse(time.RFC3339Nano, …) costs ~300ns/line
// because the layout-driven parser walks a generic state machine over the
// reference layout string. The Claude CLI exclusively emits UTC timestamps
// in the form "YYYY-MM-DDTHH:MM:SS[.fff…]Z" — a single rigid shape we can
// peel apart with byte-level integer parsing in ~30ns, an order-of-magnitude
// speedup that compounds across 500-line transcripts (250 line/s × 270ns
// saved ≈ 70µs/s reclaimed under bulk dashboard polling). Fast-path is
// guarded on the canonical shape; anything else (offsets like +08:00,
// truncated fragments, exotic layouts) falls back to time.Parse, so
// correctness is bit-for-bit identical to the previous behaviour for any
// input that isn't a perfectly-canonical UTC timestamp.
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

// parseISO8601MSFast hand-parses the canonical UTC RFC 3339 shape that
// the Claude CLI emits and returns (unixMillis, true) on success. The
// canonical shape is:
//
//	YYYY-MM-DDTHH:MM:SS(.fffffffff)?Z
//
// Any deviation (timezone offset other than 'Z', missing field, non-digit
// where digit expected, lowercase 't'/'z', etc.) returns (0, false) and
// the caller falls back to time.Parse(time.RFC3339Nano). We DO NOT
// attempt to validate calendar correctness (e.g. Feb 30) — time.Date
// performs the normalisation, matching time.Parse's tolerance for the
// same canonical shape (which also normalises rather than rejects).
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

// parseDigits parses a fixed-length all-ASCII-digits string as a non-
// negative int. Returns (n, true) on success, (0, false) on any non-digit.
// Unrolled for the common short widths (≤4 digits) we care about; longer
// inputs fall through to a tight loop. The loop variant still beats
// strconv.Atoi by avoiding the leading-sign / leading-zero dance.
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

// truncateRunes caps a string to maxBytes by rune boundary, appending
// an ellipsis indicator (3 bytes "…") when truncation actually happened.
// We trim by rune count rather than byte count so multi-byte UTF-8 sequences
// don't get split mid-codepoint (which would render as U+FFFD in the browser).
//
// R235-SEC-4: the previous implementation tested `i >= maxBytes-3` against the
// rune's *start* offset and then cut at `cum` (the previous rune's start),
// which could leave the result one rune over the cap when that earlier rune
// was multi-byte. Fixed by walking until adding the next rune would push the
// final byte length (after the "…" suffix) past maxBytes.
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

// sameFileAncestor reports whether root names the same inode as resolved or
// any of its ancestors. Used as a fallback after a byte-wise HasPrefix check
// fails so the path-escape gate honours filesystem containment semantics on
// case-insensitive filesystems (macOS APFS/HFS+ default, Windows NTFS) where
// EvalSymlinks preserves user-typed case while the kernel still treats the
// path as equivalent. Walking parents one Stat at a time bounds the work to
// path depth and avoids os-specific case-folding rules. Returns false on any
// Stat error (root deleted mid-flight, permission denied, broken chain) so a
// failed ancestor probe never weakens the byte-wise gate's negative result.
func sameFileAncestor(resolved, root string) bool {
	// R103901-SEC-8: Lstat (not Stat) so a symlink at the final path
	// component is never followed when probing inode identity. Both args
	// arrive already EvalSymlinks-resolved, so on the normal path Lstat and
	// Stat return identical inode info and SameFile semantics are unchanged
	// (including case-insensitive FS matching, which folds in the kernel
	// dir lookup, not the final-component follow). Lstat closes the
	// defence-in-depth gap where a crafted root/final-component symlink
	// could otherwise let SameFile match a target outside the subtree.
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return false
	}
	cur := filepath.Clean(resolved)
	for {
		info, err := os.Lstat(cur)
		if err == nil && os.SameFile(info, rootInfo) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur { // reached filesystem root, stop.
			return false
		}
		cur = parent
	}
}
