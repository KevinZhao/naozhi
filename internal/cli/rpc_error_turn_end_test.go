package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestRPCErrorTurnEnd pins the readLoop's recognition of a rejected turn:
// handleShimStdout only synthesizes a turn-closing result event when
// rpcErrorTurnEnd returns ok, and an unrecognised rejection leaves the session
// in state=running (#2216, where a codex turn/start error fell through to
// "skip unparseable event").
func TestRPCErrorTurnEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		wantOK  bool
		wantTag string
	}{
		{
			name:    "kiro rejection",
			err:     &TurnRejectedError{Backend: "kiro", Err: fmt.Errorf("%w -32000: model overloaded", ErrACPRPC)},
			wantOK:  true,
			wantTag: "[kiro] ",
		},
		{
			name:    "codex rejection",
			err:     &TurnRejectedError{Backend: "codex", Err: fmt.Errorf("%w -32001: Server overloaded", ErrCodexRPC)},
			wantOK:  true,
			wantTag: "[codex] ",
		},
		{
			name:    "rejection wrapped further up still counts",
			err:     fmt.Errorf("readEvent: %w", &TurnRejectedError{Backend: "codex", Err: ErrCodexRPC}),
			wantOK:  true,
			wantTag: "[codex] ",
		},
		{
			name:    "protocol built without a backend ID gets no tag",
			err:     &TurnRejectedError{Err: ErrACPRPC},
			wantOK:  true,
			wantTag: "",
		},
		{
			name:   "a bare RPC sentinel is not a rejected turn",
			err:    fmt.Errorf("%w 1: handshake", ErrCodexRPC),
			wantOK: false,
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
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rejected, ok := rpcErrorTurnEnd(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("rpcErrorTurnEnd(%v) ok = %v, want %v", tc.err, ok, tc.wantOK)
			}
			if ok && rejected.resultPrefix() != tc.wantTag {
				t.Errorf("tag = %q, want %q", rejected.resultPrefix(), tc.wantTag)
			}
		})
	}
}

// TestTurnRejectedError_KeepsTheProtocolSentinel: callers that match the
// protocol's own sentinel still can, and the message is the protocol's.
func TestTurnRejectedError_KeepsTheProtocolSentinel(t *testing.T) {
	t.Parallel()
	inner := fmt.Errorf("%w 1: boom", ErrCodexRPC)
	err := error(&TurnRejectedError{Backend: "codex", Err: inner})
	if !errors.Is(err, ErrCodexRPC) {
		t.Error("errors.Is(rejection, ErrCodexRPC) = false, want the sentinel reachable")
	}
	if errors.Is(err, ErrACPRPC) {
		t.Error("a codex rejection must not satisfy errors.Is(ErrACPRPC)")
	}
	if err.Error() != inner.Error() {
		t.Errorf("Error() = %q, want the protocol's message %q", err.Error(), inner.Error())
	}
}

// TestProtocols_TagTheRejectionWithTheirBackend: ACP and codex are protocols a
// backend picks, so a rejection is tagged with the backend the protocol was
// built for, not with the one that first used it.
func TestProtocols_TagTheRejectionWithTheirBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		proto Protocol
		frame string
		code  int
	}{
		{"acp", &ACPProtocol{BackendID: "other-acp"}, `{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"no\u0007"}}`, -32000},
		{"codex", &CodexProtocol{BackendID: "other-codex"}, `{"jsonrpc":"2.0","id":3,"error":{"code":-32001,"message":"no\u0007"}}`, -32001},
	} {
		_, _, err := tc.proto.ReadEvent(tc.frame)
		rejected, ok := rpcErrorTurnEnd(err)
		if !ok {
			t.Fatalf("%s rejection: rpcErrorTurnEnd(%v) ok = false", tc.name, err)
		}
		if want := "[other-" + tc.name + "] "; rejected.resultPrefix() != want {
			t.Errorf("%s rejection: tag = %q, want %q", tc.name, rejected.resultPrefix(), want)
		}
		// The code survives as a number and the message sanitized, so a
		// consumer classifies the rejection without parsing Error().
		if rejected.Code != tc.code || rejected.Message != "no_" {
			t.Errorf("%s rejection: Code, Message = %d, %q; want %d, %q", tc.name, rejected.Code, rejected.Message, tc.code, "no_")
		}
	}
}

// TestTurnRejectedError_ResultEventIsAStructuredFailure: the result the
// readLoop closes a rejected turn with keeps today's text, and also says it
// failed and why, for consumers that must not parse that text.
func TestTurnRejectedError_ResultEventIsAStructuredFailure(t *testing.T) {
	t.Parallel()
	rejected := &TurnRejectedError{Backend: "kiro", Code: -32000, Message: "model overloaded",
		Err: fmt.Errorf("%w -32000: model overloaded", ErrACPRPC)}
	ev := rejected.resultEvent()
	if ev.Type != "result" || ev.SubType != "error" || !ev.IsError {
		t.Errorf("Type, SubType, IsError = %q, %q, %v; want result, error, true", ev.Type, ev.SubType, ev.IsError)
	}
	if want := "[kiro] acp rpc error -32000: model overloaded"; ev.Result != want {
		t.Errorf("Result = %q, want %q", ev.Result, want)
	}
	want := clievent.BackendError{Backend: "kiro", Code: -32000, Message: "model overloaded"}
	if ev.BackendError == nil || *ev.BackendError != want {
		t.Errorf("BackendError = %+v, want %+v", ev.BackendError, want)
	}
	if got := resultFromEvent(ev); got.BackendError != ev.BackendError || !got.IsError || got.SubType != "error" {
		t.Errorf("SendResult = %+v, lost the failure", got)
	}
}
