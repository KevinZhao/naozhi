package weixin

import (
	"context"
	"encoding/json"
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
	w := New(Config{Token: "tok", BaseURL: rl.srv.URL})
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

// TestConnState_FailedPollDisconnects: a transport failure and an iLink API
// error both mark the link disconnected with the reason as LastError.
func TestConnState_FailedPollDisconnects(t *testing.T) {
	t.Parallel()
	cases := []struct {
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
				_ = json.NewEncoder(w).Encode(getUpdatesResp{Ret: -14, ErrCode: -14, ErrMsg: "session\x1b[31m timeout"})
			},
			wantErr: "getUpdates ret=-14 errcode=-14: session",
		},
	}
	for _, tc := range cases {
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
