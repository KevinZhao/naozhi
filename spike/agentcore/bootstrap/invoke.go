package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// payload is the injection envelope naozhi sends per RFC §4.1. Phase 0 spike
// keeps the minimal field set needed for V1-V4: settings + CLAUDE.md + prompt.
// Skills/MCP/secrets land with the real agentcoreRunner.
type payload struct {
	// Settings is written verbatim to ~/.claude/settings.json.
	Settings json.RawMessage `json:"settings,omitempty"`
	// ClaudeMD is written to <workdir>/CLAUDE.md when non-empty.
	ClaudeMD string `json:"claude_md,omitempty"`
	// Prompt is the single user turn (run-once job model, RFC §3.2 decision A).
	Prompt string `json:"prompt"`
	// Model overrides the CLI --model flag when non-empty.
	Model string `json:"model,omitempty"`
	// Env carries extra environment for the CLI process (e.g. AWS_REGION).
	Env map[string]string `json:"env,omitempty"`
}

// sseEvent wraps every line we stream back so the caller can split
// CLI stream-json events from bootstrap diagnostics.
type sseEvent struct {
	Kind string          `json:"kind"`           // "cli" | "boot" | "exit" | "meta" | "keepalive"
	Line json.RawMessage `json:"line,omitempty"` // raw stream-json event (kind=cli)
	Msg  string          `json:"msg,omitempty"`  // diagnostics (kind=boot/exit)
	Code int             `json:"code,omitempty"` // exit code (kind=exit)
	// ImageVersion / MemoryPeakBytes are populated only on kind=meta — the
	// microVM execution receipt the CLI stream cannot supply (RFC §7.3).
	ImageVersion    string `json:"image_version,omitempty"`
	MemoryPeakBytes int64  `json:"memory_peak_bytes,omitempty"`
	TS              string `json:"ts"`
}

func handleInvocation(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var p payload
	if err := json.NewDecoder(io.LimitReader(r.Body, 100<<20)).Decode(&p); err != nil {
		http.Error(w, fmt.Sprintf("bad payload: %v", err), http.StatusBadRequest)
		return
	}
	if p.Prompt == "" {
		http.Error(w, "payload.prompt is required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	var emitMu sync.Mutex
	emit := func(ev sseEvent) {
		ev.TS = time.Now().UTC().Format(time.RFC3339Nano)
		b, err := json.Marshal(ev)
		if err != nil {
			log.Printf("bootstrap: marshal event: %v", err)
			return
		}
		emitMu.Lock()
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		emitMu.Unlock()
	}

	// Keepalive: long tool calls (sleep/build/clone) leave the SSE stream
	// silent for minutes; AgentCore's idleRuntimeSessionTimeout judges the
	// session idle on a quiet stream and burns the microVM mid-job (observed
	// in Phase 0 V8: 60s idle timeout killed a sleeping job). Periodic
	// keepalive events keep the stream non-idle for the whole job lifetime.
	//
	// The goroutine must be fully joined before this handler returns: emit
	// writes to the ResponseWriter, and net/http forbids touching w after
	// the handler returns.
	stopKeepalive := make(chan struct{})
	var keepaliveWG sync.WaitGroup
	keepaliveWG.Add(1)
	defer keepaliveWG.Wait()
	defer close(stopKeepalive)
	go func() {
		defer keepaliveWG.Done()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				emit(sseEvent{Kind: "keepalive"})
			case <-stopKeepalive:
				return
			case <-r.Context().Done():
				return
			}
		}
	}()

	if err := materialize(p); err != nil {
		emit(sseEvent{Kind: "boot", Msg: "materialize failed: " + err.Error()})
		emit(sseEvent{Kind: "exit", Code: -1, Msg: "boot-failure"})
		return
	}
	emit(sseEvent{Kind: "boot", Msg: fmt.Sprintf("materialized in %s", time.Since(started))})

	if err := runCLI(r, p, emit); err != nil {
		emit(sseEvent{Kind: "boot", Msg: "cli failed: " + err.Error()})
	}
}

// materialize writes the injected tenant layer to the microVM filesystem
// (RFC §2.1 runtime-injection column). Everything written here burns with
// the microVM.
func materialize(p payload) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir ~/.claude: %w", err)
	}
	if len(p.Settings) > 0 {
		if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), p.Settings, 0o600); err != nil {
			return fmt.Errorf("write settings.json: %w", err)
		}
	}
	workdir := workDir()
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("mkdir workdir: %w", err)
	}
	if p.ClaudeMD != "" {
		if err := os.WriteFile(filepath.Join(workdir, "CLAUDE.md"), []byte(p.ClaudeMD), 0o644); err != nil {
			return fmt.Errorf("write CLAUDE.md: %w", err)
		}
	}
	return nil
}

func workDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/job"
	}
	return filepath.Join(home, "job")
}

// runCLI spawns the long-lived claude CLI exactly like naozhi's local spawn
// does (stream-json over stdin/stdout, mirrors internal/cli BuildArgs), feeds
// the single prompt, and relays every stdout line as an SSE event. Run-once:
// after the result event the CLI is torn down and the handler returns, which
// lets the control plane close out and the microVM die by StopRuntimeSession
// or idle timeout.
func runCLI(r *http.Request, p payload, emit func(sseEvent)) error {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		// Sandbox is single-tenant-per-microVM; the injected settings.json is
		// the only config source, mirroring naozhi's `--setting-sources user`.
		"--setting-sources", "user",
	}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}

	ctx := r.Context()
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workDir()
	cmd.Env = os.Environ()
	for k, v := range p.Env {
		// Reject keys that could splice extra variables or shadow the base
		// env in implementation-defined ways (empty key, '=' inside key).
		if k == "" || strings.ContainsRune(k, '=') {
			emit(sseEvent{Kind: "boot", Msg: "rejected env key: " + k})
			continue
		}
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn claude: %w", err)
	}
	emit(sseEvent{Kind: "boot", Msg: fmt.Sprintf("claude spawned pid=%d", cmd.Process.Pid)})

	// Feed the single user turn, then close stdin: with -p stream-json the
	// CLI exits on its own after the result event once stdin is closed.
	userMsg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": p.Prompt}},
		},
	}
	msgBytes, err := json.Marshal(userMsg)
	if err != nil {
		stdin.Close()
		_ = cmd.Process.Kill()
		return fmt.Errorf("marshal user message: %w", err)
	}
	if _, err := stdin.Write(append(msgBytes, '\n')); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("write prompt: %w", err)
	}
	stdin.Close()

	// Drain stderr in the background so the CLI can't block on a full pipe;
	// surface it as boot diagnostics (truncated per line). Joined before
	// return: emit touches the ResponseWriter, which must not be used after
	// the handler returns; the pipe EOFs when the CLI exits so the join
	// cannot hang. Must complete before cmd.Wait() (Wait closes the pipes).
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			line := sc.Text()
			if len(line) > 512 {
				line = line[:512]
			}
			emit(sseEvent{Kind: "boot", Msg: "stderr: " + line})
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		raw := sc.Bytes()
		if !json.Valid(raw) {
			emit(sseEvent{Kind: "boot", Msg: "non-json stdout line, skipped"})
			continue
		}
		line := make(json.RawMessage, len(raw))
		copy(line, raw)
		emit(sseEvent{Kind: "cli", Line: line})
	}
	if err := sc.Err(); err != nil {
		emit(sseEvent{Kind: "boot", Msg: "stdout scan: " + err.Error()})
	}

	stderrWG.Wait()
	// Peak memory (RFC §7.3): sample BEFORE cmd.Wait() reaps the child — once
	// reaped, /proc/<cli-pid>/status is gone. Prefer the cgroup peak (covers
	// the whole claude + MCP subtree, which is what 8GB-per-microVM accounting
	// actually cares about); fall back to the CLI parent's VmHWM. Reading our
	// own /proc/self (the prior shape) measured the wrong process — the
	// bootstrap's ~7MB RSS, not the CLI's ~250MB+ (review PR-1 F4).
	memPeak := peakRSSBytes(cmd.Process.Pid)
	waitErr := cmd.Wait()
	code := 0
	if waitErr != nil {
		code = cmd.ProcessState.ExitCode()
	}
	// Execution receipt (RFC §7.3): emit image version + peak RSS just before
	// the exit frame. Both are best-effort — a read failure omits the field
	// rather than failing the run; the agentcore client
	// treats zero values as "unknown".
	emit(sseEvent{
		Kind:            "meta",
		ImageVersion:    imageVersion(),
		MemoryPeakBytes: memPeak,
	})
	emit(sseEvent{Kind: "exit", Code: code, Msg: "cli-exited"})
	return waitErr
}

// imageVersion returns the baked base-image tag, stamped at build time via
// the NAOZHI_SANDBOX_IMAGE_VERSION env (set in the Dockerfile / runtime
// config). Empty when unset — the client records "unknown".
func imageVersion() string {
	return os.Getenv("NAOZHI_SANDBOX_IMAGE_VERSION")
}

// peakRSSBytes returns the peak memory of the CLI job for the run record
// (RFC §7.3). It prefers the cgroup memory peak — in a microVM the cgroup
// spans the whole claude + MCP subtree, which is what the 8GB-per-session
// ceiling (§1.3) actually constrains — and falls back to the CLI parent's
// VmHWM (cliPID's /proc status) when no cgroup peak file exists. Caller
// MUST invoke this BEFORE cmd.Wait() reaps cliPID, otherwise the proc
// fallback reads a dead PID. Linux-only, best-effort: any failure yields 0,
// which the client records as "unknown" and omits.
func peakRSSBytes(cliPID int) int64 {
	if v := cgroupMemoryPeak(); v > 0 {
		return v
	}
	return procVmHWM(cliPID)
}

// cgroupMemoryPeak reads memory.peak (cgroup v2). Returns 0 when absent
// (cgroup v1, no cgroupfs, or the kernel predates memory.peak).
func cgroupMemoryPeak() int64 {
	for _, p := range []string{
		"/sys/fs/cgroup/memory.peak",                      // v2 unified, our cgroup
		"/sys/fs/cgroup/memory/memory.max_usage_in_bytes", // v1 fallback
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// procVmHWM reads VmHWM (peak RSS) from /proc/<pid>/status for the CLI
// process. Note this captures only the claude parent, not its MCP children
// — that is why cgroupMemoryPeak is preferred. Best-effort: 0 on any
// read/parse failure.
func procVmHWM(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "VmHWM:")
		if !ok {
			continue
		}
		// Format: "VmHWM:\t   12345 kB"
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
