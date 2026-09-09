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
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/claudefs"
	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/osutil"
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

// GET /api/cron/runs/{run_id}/transcript?job_id=<jid>
func (h *Handlers) HandleRunTranscript(w http.ResponseWriter, r *http.Request) {
	// Use the dedicated transcriptLimiter rather than the shared runsLimiter:
	// the transcript path fans out far more I/O (EvalSymlinks ×2 + 8 MB
	// LimitReader + 256 KB scanner + per-line Unmarshal), so one shared bucket
	// would let either endpoint starve the other (#1096). runsLimiter is the
	// fallback for hand-rolled fixtures without a transcriptLimiter.
	limiter := h.deps.RateLimits.Transcript
	if limiter == nil {
		limiter = h.deps.RateLimits.Runs
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
	if h.deps.Scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	runID, jobID, ok := parseRunPathParams(w, r)
	if !ok {
		return
	}

	run, err := h.deps.Scheduler.Run(jobID, runID)
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
	if run.SessionID == "" || h.deps.ClaudeDir == "" || run.WorkDir == "" {
		resp.Fallback = "missing"
		httputil.WriteJSON(w, resp)
		return
	}
	if !claudefs.IsValidSessionID(run.SessionID) {
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
	// Defence in depth before claudefs.ProjectSlug encodes WorkDir into a path
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

	jsonlPath := claudefs.SessionJSONL(h.deps.ClaudeDir, run.WorkDir, run.SessionID)

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
	allowedRoot := claudefs.ProjectsRoot(h.deps.ClaudeDir)
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
		slog.Warn("cron transcript: path escape attempt", "raw", jsonlPath, "resolved", resolved, "claudeDir", h.deps.ClaudeDir, "allowedRoot", resolvedRoot)
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
		var ev claudefs.Line
		if err := json.Unmarshal(line, &ev); err != nil {
			// Skip unparseable line; do not fail the whole response.
			continue
		}
		// Time-window filter applies only to dated events. For fresh=false the
		// JSONL is shared across cron runs and we have no per-event run-id, so
		// timestamp-less events ("queue-operation", untimestamped attachments)
		// are dropped rather than leaked into an adjacent run's transcript
		// (#1046). fresh=true runs own the JSONL, so they pass through there.
		ts := claudefs.TimestampMillis(ev.Timestamp)
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
