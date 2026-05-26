package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"
)

// buildShimArgs constructs the argv slice handed to the naozhi binary's
// `shim run` subcommand. Extracted from StartShimWithBackend per R237-CR-11
// / R246-CR-005 so the spawn function can stay focused on slot bookkeeping
// and handle install. Argv layout intentionally matches the order accepted
// by cmd/naozhi/shim.go — keep them in sync.
//
// R246-CR-007: integers are formatted via strconv to avoid the fmt reflect
// path on every spawn. bufferSize is int; maxBufBytes is int64.
func (m *Manager) buildShimArgs(key, socketPath, stateFile, cliPath, backend string, cliArgs []string, cwd string) []string {
	args := []string{"shim", "run",
		"--key", key,
		"--socket", socketPath,
		"--state-file", stateFile,
		"--buffer-size", strconv.Itoa(m.bufferSize),
		"--max-buffer-bytes", strconv.FormatInt(m.maxBufBytes, 10),
		"--idle-timeout", m.idleTimeout.String(),
		"--watchdog-timeout", m.watchdogTimeout.String(),
		"--cli-path", cliPath,
		"--cwd", cwd,
	}
	if backend != "" {
		args = append(args, "--backend", backend)
	}
	for _, a := range cliArgs {
		args = append(args, "--cli-arg", a)
	}
	return args
}

// waitForShimReady reads the JSON ready frame from the shim's stdout pipe
// and returns the base64 token on success. On parse failure / status=error
// / startup timeout / ctx cancellation it invokes onFail (typically
// killAndUnblock) so the shim is reaped and the inner scanner goroutine
// can deliver to the buffered readyCh and exit.
//
// Extracted from StartShimWithBackend per R237-CR-11 / R246-CR-005 so the
// 30s handshake timeout selection is a single named function instead of an
// inline goroutine + select inside a 200-line spawn flow. The caller still
// owns stdout (this function only reads from it) — closing on the failure
// path is the caller's onFail responsibility, kept symmetric with the
// connect+cgroup branches that share the same cleanup helper.
//
// Use NewTimer + defer Stop so the goroutine backing time.After does not
// park for 30s after a fast-path success or ctx cancellation. Under high
// start/restart pressure this previously accumulated up to thousands of
// live timer goroutines between GC cycles.
func waitForShimReady(ctx context.Context, stdout io.ReadCloser, timeout time.Duration, onFail func()) (string, error) {
	readyCh := make(chan shimReadyMsg, 1)
	go func() {
		defer stdout.Close()
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			var ready struct {
				Status string `json:"status"`
				PID    int    `json:"pid"`
				Token  string `json:"token"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &ready); err != nil {
				readyCh <- shimReadyMsg{"", fmt.Errorf("parse ready: %w", err)}
				return
			}
			if ready.Status == "error" {
				readyCh <- shimReadyMsg{"", fmt.Errorf("shim startup failed: %s", ready.Error)}
				return
			}
			if ready.Status != "ready" {
				readyCh <- shimReadyMsg{"", fmt.Errorf("unexpected status: %s", ready.Status)}
				return
			}
			readyCh <- shimReadyMsg{ready.Token, nil}
		} else {
			readyCh <- shimReadyMsg{"", fmt.Errorf("shim exited before ready")}
		}
	}()

	readyTimer := time.NewTimer(timeout)
	defer readyTimer.Stop()

	select {
	case result := <-readyCh:
		if result.err != nil {
			onFail()
			return "", result.err
		}
		return result.token, nil
	case <-readyTimer.C:
		onFail()
		return "", fmt.Errorf("shim ready timeout")
	case <-ctx.Done():
		onFail()
		return "", ctx.Err()
	}
}
