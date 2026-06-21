package cli

import (
	"errors"
	"fmt"
	"testing"
)

// TestRPCErrorTurnEnd pins the readLoop's RPC-error-sentinel recognition
// (#2216). handleShimStdout only synthesizes a turn-closing result event when
// rpcErrorTurnEnd returns ok; before the fix it inlined errors.Is(err,
// ErrACPRPC) only, so a codex turn/start error (wrapping ErrCodexRPC) fell
// through to "skip unparseable event" and the session hung in state=running.
func TestRPCErrorTurnEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		wantOK  bool
		wantTag string
	}{
		{
			name:    "ACP RPC error wraps ErrACPRPC",
			err:     fmt.Errorf("%w -32000: model overloaded", ErrACPRPC),
			wantOK:  true,
			wantTag: "[kiro] ",
		},
		{
			name:    "codex RPC error wraps ErrCodexRPC",
			err:     fmt.Errorf("%w -32001: Server overloaded", ErrCodexRPC),
			wantOK:  true,
			wantTag: "[codex] ",
		},
		{
			name:   "bare ErrCodexRPC sentinel",
			err:    ErrCodexRPC,
			wantOK: true, wantTag: "[codex] ",
		},
		{
			name:   "bare ErrACPRPC sentinel",
			err:    ErrACPRPC,
			wantOK: true, wantTag: "[kiro] ",
		},
		{
			name:   "unrelated parse error is not a turn-end",
			err:    errors.New("unexpected end of JSON input"),
			wantOK: false,
		},
		{
			name:   "nil error",
			err:    nil,
			wantOK: false,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tag, ok := rpcErrorTurnEnd(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("rpcErrorTurnEnd(%v) ok = %v, want %v", tc.err, ok, tc.wantOK)
			}
			if ok && tag != tc.wantTag {
				t.Errorf("tag = %q, want %q", tag, tc.wantTag)
			}
		})
	}
}

// TestRPCErrorTurnEnd_CodexNotConfusedWithACP guards the exact bug: the codex
// sentinel must NOT require the ACP sentinel to be recognised. errors.Is
// across the two distinct sentinels is false, which is why the old
// ACP-only check dropped codex errors.
func TestRPCErrorTurnEnd_CodexNotConfusedWithACP(t *testing.T) {
	t.Parallel()
	codexErr := fmt.Errorf("%w 1: boom", ErrCodexRPC)
	if errors.Is(codexErr, ErrACPRPC) {
		t.Fatal("ErrCodexRPC must not satisfy errors.Is(ErrACPRPC) — the two are distinct sentinels")
	}
	if _, ok := rpcErrorTurnEnd(codexErr); !ok {
		t.Fatal("codex RPC error must be recognised as a turn-end (regression #2216)")
	}
}
