package weixin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const connStateTestTimeout = 5 * time.Second

// scriptedRelay is a fake iLink relay that answers getupdates one scripted
// reply at a time. Each poll announces itself on arrived before waiting for
// its reply, so the test can read the adapter's state between two rounds; a
// poll that gets no reply is parked until the client cancels it.
type scriptedRelay struct {
	srv     *httptest.Server
	arrived chan struct{}
	script  chan func(http.ResponseWriter)
}

func newScriptedRelay(t *testing.T) *scriptedRelay {
	t.Helper()
	rl := &scriptedRelay{
		arrived: make(chan struct{}),
		script:  make(chan func(http.ResponseWriter)),
	}
	rl.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drained first so a client cancel reaches r.Context (see holdLongPoll).
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case rl.arrived <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		select {
		case reply := <-rl.script:
			reply(w)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(rl.srv.Close)
	return rl
}

func (rl *scriptedRelay) awaitPoll(t *testing.T) {
	t.Helper()
	select {
	case <-rl.arrived:
	case <-time.After(connStateTestTimeout):
		t.Fatal("timed out waiting for a getupdates poll")
	}
}

func replyOK(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0})
}

func startScripted(t *testing.T, rl *scriptedRelay) *Weixin {
	t.Helper()
	return startScriptedWith(t, rl, func(*Weixin) {})
}

// startScriptedWith lets the test set unexported knobs before Start.
func startScriptedWith(t *testing.T, rl *scriptedRelay, configure func(*Weixin)) *Weixin {
	t.Helper()
	w := New(Config{Token: "tok", BaseURL: rl.srv.URL})
	configure(w)
	if err := w.Start(func(context.Context, platform.IncomingMessage) {}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = w.Stop() })
	return w
}

func mustConnState(t *testing.T, w *Weixin) platform.ConnState {
	t.Helper()
	s, ok := w.ConnState()
	if !ok {
		t.Fatal("ConnState not observable")
	}
	return s
}

// TestConnState_LongPollLifecycle: connecting from Start until the first
// getUpdates answers, connected after it, and Stop cancelling the parked poll
// is not recorded as a drop.
func TestConnState_LongPollLifecycle(t *testing.T) {
	t.Parallel()
	rl := newScriptedRelay(t)
	w := New(Config{Token: "tok", BaseURL: rl.srv.URL})
	if s, ok := w.ConnState(); ok {
		t.Fatalf("unstarted adapter reported %+v, want not observable", s)
	}
	if err := w.Start(func(context.Context, platform.IncomingMessage) {}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = w.Stop() }) // a failed assertion must not park srv.Close

	rl.awaitPoll(t)
	if s := mustConnState(t, w); s.State != platform.ConnConnecting {
		t.Fatalf("before the first reply: state %q, want connecting", s.State)
	}

	rl.script <- replyOK
	rl.awaitPoll(t) // the next round has started, so the first one is recorded
	connected := mustConnState(t, w)
	if connected.State != platform.ConnConnected || connected.LastError != "" {
		t.Fatalf("after a successful poll: %+v, want connected with no error", connected)
	}

	// The second poll stays parked; Stop has to cancel it.
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if s := mustConnState(t, w); s != connected {
		t.Fatalf("Stop changed the state: %+v, want %+v", s, connected)
	}
}

// TestConnState_FailedPollDisconnects: a transport failure and a non-terminal
// iLink API error both mark the link disconnected with the reason as LastError.
var failedPollReplies = []struct {
	name    string
	reply   func(http.ResponseWriter)
	wantErr string
}{
	{
		name: "http status",
		reply: func(w http.ResponseWriter) {
			http.Error(w, "relay down", http.StatusBadGateway)
		},
		wantErr: "http 502: relay down",
	},
	{
		name: "api error",
		reply: func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(getUpdatesResp{Ret: -1, ErrCode: -1, ErrMsg: "system\x1b[31m busy"})
		},
		wantErr: "getUpdates ret=-1 errcode=-1: system",
	},
}

func TestConnState_FailedPollDisconnects(t *testing.T) {
	t.Parallel()
	for _, tc := range failedPollReplies {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rl := newScriptedRelay(t)
			w := startScripted(t, rl)

			rl.awaitPoll(t)
			rl.script <- replyOK
			rl.awaitPoll(t)
			connected := mustConnState(t, w)
			if connected.State != platform.ConnConnected {
				t.Fatalf("after a successful poll: state %q, want connected", connected.State)
			}

			rl.script <- tc.reply
			testhelper.Eventually(t, func() bool {
				s, _ := w.ConnState()
				return s.State == platform.ConnDisconnected
			}, connStateTestTimeout, "state never became disconnected after a failed poll")

			s := mustConnState(t, w)
			if !strings.Contains(s.LastError, tc.wantErr) {
				t.Errorf("LastError = %q, want it to contain %q", s.LastError, tc.wantErr)
			}
			if strings.ContainsRune(s.LastError, '\x1b') {
				t.Errorf("LastError carries a raw control byte: %q", s.LastError)
			}
			if s.LastErrorAt.IsZero() || s.Since.Before(connected.Since) {
				t.Errorf("timestamps not stamped by the failure: %+v (connected since %v)", s, connected.Since)
			}
		})
	}
}

// TestConnState_RecoversAfterFailedPoll: the next successful round puts the
// link back to connected and keeps the failure visible as LastError. It pays
// pollLoop's 2s retry delay once.
func TestConnState_RecoversAfterFailedPoll(t *testing.T) {
	t.Parallel()
	rl := newScriptedRelay(t)
	w := startScripted(t, rl)

	rl.awaitPoll(t)
	rl.script <- func(w http.ResponseWriter) { http.Error(w, "relay down", http.StatusBadGateway) }
	rl.awaitPoll(t) // arrives after the retry delay
	if s := mustConnState(t, w); s.State != platform.ConnDisconnected {
		t.Fatalf("after a failed poll: state %q, want disconnected", s.State)
	}

	rl.script <- replyOK
	rl.awaitPoll(t)
	s := mustConnState(t, w)
	if s.State != platform.ConnConnected {
		t.Fatalf("after recovering: state %q, want connected", s.State)
	}
	if !strings.Contains(s.LastError, "http 502") {
		t.Errorf("recovery dropped LastError: %q", s.LastError)
	}
}

// TestConnState_RepeatedFailureKeepsSince: a relay that keeps failing stays
// disconnected with Since at the first failure, so the outage ages (doctor's
// reconnect grace relies on it), while LastErrorAt follows each failure.
func TestConnState_RepeatedFailureKeepsSince(t *testing.T) {
	t.Parallel()
	for _, tc := range failedPollReplies {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rl := newScriptedRelay(t)
			w := startScripted(t, rl)

			rl.awaitPoll(t)
			rl.script <- tc.reply
			rl.awaitPoll(t) // arrives after the retry delay
			first := mustConnState(t, w)
			if first.State != platform.ConnDisconnected {
				t.Fatalf("after one failed poll: state %q, want disconnected", first.State)
			}

			rl.script <- tc.reply
			testhelper.Eventually(t, func() bool {
				s, _ := w.ConnState()
				return s.LastErrorAt.After(first.LastErrorAt)
			}, connStateTestTimeout, "the second failed poll was never recorded")

			second := mustConnState(t, w)
			if second.State != platform.ConnDisconnected || !second.Since.Equal(first.Since) {
				t.Errorf("after a second failed poll: %+v, want disconnected since %v", second, first.Since)
			}
		})
	}
}

func replyAPIError(ret, errCode int, msg string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(getUpdatesResp{Ret: ret, ErrCode: errCode, ErrMsg: msg})
	}
}

// TestConnState_StaleTokenFails: -14 in either ret or errcode is an expired
// bot token, reported as failed with the re-login hint first. Polling goes on
// after staleTokenPause rather than the 2s retry, and a later success
// recovers to connected without dropping the error.
func TestConnState_StaleTokenFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		ret, errCode int
	}{
		{"ret only", -14, 0},
		{"errcode only", 0, -14},
		{"both", -14, -14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rl := newScriptedRelay(t)
			w := startScriptedWith(t, rl, func(w *Weixin) { w.staleTokenPause = 50 * time.Millisecond })

			rl.awaitPoll(t)
			rl.script <- replyOK
			rl.awaitPoll(t)

			rl.script <- replyAPIError(tc.ret, tc.errCode, "session timeout")
			replied := time.Now()
			rl.awaitPoll(t)
			// Well under pollLoop's 2s retryDelay, so the pause is what ran.
			if waited := time.Since(replied); waited > time.Second {
				t.Errorf("next poll came %v after the stale-token reply, want the %v pause", waited, w.staleTokenPause)
			}
			s := mustConnState(t, w)
			if s.State != platform.ConnFailed {
				t.Fatalf("after a stale-token reply: state %q, want failed", s.State)
			}
			want := fmt.Sprintf("weixin token expired (iLink -14): run 'naozhi setup weixin' "+
				"(restart unless it reconnects by itself): getUpdates ret=%d errcode=%d: session timeout",
				tc.ret, tc.errCode)
			if s.LastError != want {
				t.Errorf("LastError = %q, want %q", s.LastError, want)
			}

			rl.script <- replyOK
			rl.awaitPoll(t)
			recovered := mustConnState(t, w)
			if recovered.State != platform.ConnConnected || recovered.LastError != s.LastError {
				t.Errorf("after the token works again: %+v, want connected keeping %q", recovered, s.LastError)
			}
		})
	}
}

// TestConnState_StaleTokenResetsFailureStreak: a stale-token reply ends the
// run of failures, so the failure after it waits the 2s retry and not the 30s
// backoff that a third strike would take. It pays the 2s retry three times.
func TestConnState_StaleTokenResetsFailureStreak(t *testing.T) {
	t.Parallel()
	rl := newScriptedRelay(t)
	startScriptedWith(t, rl, func(w *Weixin) { w.staleTokenPause = 50 * time.Millisecond })

	busy := replyAPIError(-1, -1, "busy")
	for _, reply := range []func(http.ResponseWriter){busy, busy, replyAPIError(-14, -14, "session timeout"), busy} {
		rl.awaitPoll(t)
		rl.script <- reply
	}
	rl.awaitPoll(t) // times out if the last failure took the backoff
}

func TestStaleTokenRetryDelay_DefaultsToAnHour(t *testing.T) {
	t.Parallel()
	if got := New(Config{Token: "tok"}).staleTokenRetryDelay(); got != time.Hour {
		t.Errorf("default stale-token pause = %v, want 1h", got)
	}
}
