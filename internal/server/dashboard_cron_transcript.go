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
	"strings"
	"time"
	"unicode/utf8"

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

// truncatedToolInputPlaceholder is the JSON value substituted for
// tool_use.Input fields that exceed maxToolInputBytes. Pre-encoded so the
// hot path never re-marshals; must be a valid JSON value (a string
// literal here) so the wire shape stays consistent for dashboard JS.
var truncatedToolInputPlaceholder = json.RawMessage(`"[truncated]"`)

// transcriptScanInflight bounds concurrent transcript-scan handlers globally
// across the process. R243-SEC-12 (#798): each in-flight call holds a 256 KB
// bufio.Scanner buffer plus a 32 KB output-aggregation slice plus the decoded
// transcriptResponse — call it ~512 KB resident per request once the scanner
// fully fills. Without a cap, N concurrent operators × M browser tabs each
// hammering /api/cron/runs/.../transcript can push hundreds of MB of resident
// scanner state through a server that's otherwise designed for a single
// operator.
//
// runsLimiter (per-IP rate) bounds *frequency* per source — but concurrent
// in-flight memory is orthogonal: a single IP firing 64 parallel fetch()
// calls fits under any sane rate-limit budget yet still pins half a GB of
// live scanner buffers until the writes drain. Cap the parallel scans to a
// small number (16) chosen so:
//
//	16 × ~512KB ≈ 8MB worst-case resident scanner state, well below the
//	naozhi single-binary memory budget even on the smallest deployments.
//
// Channel-based semaphore (buffered channel of size 16) is preferred over
// golang.org/x/sync/semaphore.Weighted because (a) we don't need a context
// timeout — handlers should fail fast rather than queue, and (b) it adds
// zero new dependencies to the package.
//
// On full bucket: reply with 503 Service Unavailable + Retry-After: 1 so the
// dashboard's fetch wrapper backs off gracefully. Status 503 (not 429) keeps
// the per-IP rate-limit signal distinguishable from the cross-IP
// concurrency-cap signal so operators can tell which gate fired.
const transcriptScanInflightCap = 16

var transcriptScanInflight = make(chan struct{}, transcriptScanInflightCap)

// acquireTranscriptScanSlot returns true and reserves a slot if one is
// available; returns false immediately when all slots are in use. Callers
// MUST call releaseTranscriptScanSlot exactly once on the success path.
func acquireTranscriptScanSlot() bool {
	select {
	case transcriptScanInflight <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseTranscriptScanSlot() {
	// Buffered receive — never blocks because acquire only succeeds on
	// successful send, so the counter is always ≥ 1 when this is called.
	<-transcriptScanInflight
}

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
type transcriptTurn struct {
	Index      int             `json:"index"`
	Kind       string          `json:"kind"` // "user" | "assistant" | "tool_use" | "tool_result" | "error"
	TS         int64           `json:"ts,omitempty"`
	Text       string          `json:"text,omitempty"`        // user / assistant / error
	Tokens     int             `json:"tokens,omitempty"`      // assistant only (output token delta)
	Tool       string          `json:"tool,omitempty"`        // tool_use
	ToolUseID  string          `json:"tool_use_id,omitempty"` // tool_use / tool_result link
	Summary    string          `json:"summary,omitempty"`     // tool_use one-liner derived from input
	Input      json.RawMessage `json:"input,omitempty"`       // tool_use raw input (object)
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
func (h *CronHandlers) handleRunTranscript(w http.ResponseWriter, r *http.Request) {
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	// R243-SEC-12 (#798): cap concurrent in-flight transcript scans across
	// the whole process to bound resident scanner-buffer memory under
	// multi-operator load. See transcriptScanInflight godoc for the
	// budget rationale. Distinct from runsLimiter (per-IP rate) so
	// operators can tell concurrency-cap (503) and rate-limit (429)
	// signals apart in logs.
	if !acquireTranscriptScanSlot() {
		w.Header().Set("Retry-After", "1")
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "transcript readers busy, retry shortly"})
		return
	}
	defer releaseTranscriptScanSlot()
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
		writeJSON(w, resp)
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
			writeJSON(w, resp)
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
			writeJSON(w, resp)
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
			if ts < startedMS || ts > endedMS {
				continue
			}
		} else if !run.Fresh {
			// Shared JSONL + no timestamp ⇒ cannot attribute to this
			// run; skip rather than leak adjacent-run state.
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
			slog.Warn("cron transcript: line too long (returning partial)", "path", resolved, "err", err)
			setTruncated("line_too_long")
		} else {
			slog.Warn("cron transcript: scan io error (returning partial)", "path", resolved, "err", err)
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

// sanitizeWireText drops bidi / C1 / LS-PS runes (the IsLogInjectionRune
// class) before transcript turn fields reach the JSON wire. Preserves
// \t / \n / \r so multi-line tool_result rendering survives — calling
// SanitizeForLog directly would map those to '_' and destroy formatting
// in the dashboard's <pre> sink.
//
// R243-SEC-5: handleRunDetail runs Prompt/WorkDir through the strict
// SanitizeForLog before wire-encode; the JSONL transcript path skipped
// sanitisation entirely, so a JSONL file with bidi overrides could reach
// the dashboard verbatim and corrupt visual ordering despite esc()-then-
// <pre>. Defence-in-depth.
func sanitizeWireText(s string) string {
	if s == "" {
		return s
	}
	// Fast path: every IsLogInjectionRune codepoint encodes with leading
	// byte ≥ 0x80 in UTF-8 (C1 0x80..9F → 0xC2..; bidi 0x202A..2069 →
	// 0xE2..). Pure ASCII proves nothing to drop, so return s without
	// allocating. Keeps tab/newline/CR which are < 0x20 ASCII.
	hasNonASCII := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			hasNonASCII = true
			break
		}
	}
	if !hasNonASCII {
		return s
	}
	return strings.Map(func(r rune) rune {
		if osutil.IsLogInjectionRune(r) {
			return -1 // drop
		}
		return r
	}, s)
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
// Unmarshal failures are logged at Debug — we want operator visibility
// without changing the downgrade fallback.
func flattenSystemEvent(ev *claudeJSONLEvent, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	out := make([]transcriptTurn, 0, 1)
	tok := transcriptTokens{}

	var sys struct {
		Subtype string `json:"subtype"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(ev.Message, &sys); err != nil {
		// Don't change downgrade behavior — sys stays zero-valued so
		// the error-turn branch below is skipped naturally. Just surface
		// the parse failure for ops visibility.
		slog.Debug("cron transcript: system event unmarshal failed; skipping",
			"err", err)
	}
	if sys.Subtype != "error" || sys.Message == "" {
		return out, tok, 0, false
	}
	out = append(out, transcriptTurn{
		Index: nextIdx,
		Kind:  "error",
		TS:    ts,
		Text:  sanitizeWireText(truncateRunes(sys.Message, maxAssistantTextBytes)),
	})
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

// maxToolInputProbeBytes caps the JSON byte length summariseToolInput will
// attempt to decode. R242-SEC-13 / #645: a transcript line carrying a
// pathological tool_use input (deeply-nested JSON or a megabyte string in
// an unrecognised key) would force json.Unmarshal to walk the full payload
// even though we only emit a 200-byte header label. The decoder allocates
// on every nested level it encounters, so an attacker who can write a
// transcript can amplify a single header render into a multi-MB scan.
//
// 64 KiB is comfortably above any realistic tool-input recognised here
// (Bash command + file_path are typically <1 KiB) while bounding the
// worst-case decoder cost to a fixed budget. Lines exceeding the cap fall
// through to the truncated-input fallback so the dashboard still shows a
// useful header (the sanitizer below already enforces the 200-byte output
// ceiling).
const maxToolInputProbeBytes = 64 * 1024

// summariseToolInput builds a one-line label for the tool_use card
// header. Best-effort: Bash → command, Read/Write/Edit → file_path,
// otherwise fall back to a JSON-trimmed dump of the input.
//
// R242-SEC-13 / #645: oversized inputs are truncated before json.Unmarshal
// so a malicious transcript line cannot amplify the per-line decode cost
// past a fixed budget. The output is independently capped at 200 bytes by
// SanitizeForLog so wire output stays bounded either way.
func summariseToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	if len(input) > maxToolInputProbeBytes {
		// Skip the probe decode entirely on oversized payloads — the
		// decoder's reflect cost grows with input length × nesting and
		// the typed probe only carries six top-level string fields, so
		// the work is wasted on a payload we will fall back to truncating
		// anyway. SanitizeForLog still applies its 200-byte cap on the
		// truncated head bytes.
		return osutil.SanitizeForLog(string(input[:maxToolInputProbeBytes]), 200)
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
			return osutil.SanitizeForLog(s, 200)
		}
	}
	// Fallback: reuse the original input bytes (json.Unmarshal does not
	// mutate its source). The probe struct only validated structure, no
	// need to Marshal again. R244-GO-P2-2.
	return osutil.SanitizeForLog(string(input), 200)
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
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	cur := filepath.Clean(resolved)
	for {
		info, err := os.Stat(cur)
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
