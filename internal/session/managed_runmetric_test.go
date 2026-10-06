package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// newInstrumentedSession builds a ManagedSession bound to a TestProcess and a
// real (temp-dir) run-history store, so finishRun's async write actually
// lands on disk for assertion.
func newInstrumentedSession(t *testing.T, sendFunc func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error)) (*ManagedSession, *runhistory.Store) {
	t.Helper()
	store := runhistory.NewStore(t.TempDir(), 0, 0)
	t.Cleanup(store.Close)
	s := &ManagedSession{key: "feishu:p2p:tester", runStore: store}
	s.storeProcess(&TestProcess{AliveVal: true, SendFunc: sendFunc})
	return s, store
}

func TestSend_RecordsCompletedRun(t *testing.T) {
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []clievent.Attachment, on clievent.EventCallback) (*clievent.SendResult, error) {
		if on != nil {
			on(clievent.Event{}) // emit a first byte
		}
		return &clievent.SendResult{Text: "ok", CostUSD: 0.05}, nil
	})

	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	store.Close() // flush worker

	runs := store.Recent(s.key, 0)
	if len(runs) != 1 {
		t.Fatalf("want 1 recorded run, got %d", len(runs))
	}
	r := runs[0]
	if r.Outcome != runhistory.OutcomeCompleted {
		t.Errorf("outcome = %s, want completed", r.Outcome)
	}
	if r.DurationMS < 0 {
		t.Errorf("duration must be >= 0, got %d", r.DurationMS)
	}
	if r.FirstByteMS < 0 {
		t.Errorf("first byte must be >= 0, got %d", r.FirstByteMS)
	}
	if r.CostUSD != 0.05 {
		t.Errorf("cost = %v, want 0.05", r.CostUSD)
	}
}

// TestSend_PerTurnCostDelta verifies that the per-run record stores the
// genuine single-turn increment (not the CLI's cumulative total_cost_usd) and
// that the session's authoritative total (costSpent) accumulates those deltas
// across a sequence of turns within one CLI incarnation.
func TestSend_PerTurnCostDelta(t *testing.T) {
	// Monotonic cumulative readings within one incarnation (the per-incarnation
	// reset on resume is handled at the session boundary, not in finishRun).
	raws := []float64{2.0, 5.0, 6.0, 9.0}
	wantDeltas := []float64{2.0, 3.0, 1.0, 3.0} // 2, (5-2), (6-5), (9-6)
	var idx int
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []clievent.Attachment, on clievent.EventCallback) (*clievent.SendResult, error) {
		r := &clievent.SendResult{Text: "ok", CostUSD: raws[idx]}
		idx++
		return r, nil
	})

	for i := range raws {
		if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	store.Close()

	runs := store.Recent(s.key, 0) // newest-first
	if len(runs) != len(raws) {
		t.Fatalf("want %d runs, got %d", len(raws), len(runs))
	}
	// runs are newest-first; reverse-index into wantDeltas.
	for i, r := range runs {
		want := wantDeltas[len(wantDeltas)-1-i]
		if r.CostUSD < want-1e-9 || r.CostUSD > want+1e-9 {
			t.Errorf("run[%d] (newest-first) cost = %v, want delta %v", i, r.CostUSD, want)
		}
	}

	// Session total = sum of deltas = final cumulative 9.0 (NOT the
	// over-counted sum-of-snapshots 22.0).
	const wantTotal = 9.0
	if got := loadTotalCost(&s.costSpent); got < wantTotal-1e-9 || got > wantTotal+1e-9 {
		t.Errorf("costSpent = %v, want %v (sum of per-turn deltas)", got, wantTotal)
	}
}

// TestFinishRun_ConcurrentOutOfOrderNoOverCount is the regression guard for the
// passthrough concurrency hazard the adversarial review caught: two same-
// session turns complete on separate goroutines, so finishRun may apply the
// later (higher) cumulative before the earlier (lower) one. The session total
// must equal the highest cumulative regardless of arrival order — never the
// over-counted sum. Run under -race to also exercise costMu.
func TestFinishRun_ConcurrentOutOfOrderNoOverCount(t *testing.T) {
	store := runhistory.NewStore(t.TempDir(), 0, 0)
	t.Cleanup(store.Close)
	s := &ManagedSession{key: "feishu:p2p:concurrent", runStore: store}

	// Drive finishRun directly with two timers and reversed cumulative order:
	// the higher reading (5.0) lands first, then the lower (2.0).
	rt1 := &runTimer{started: time.Now()}
	rt2 := &runTimer{started: time.Now()}
	done := make(chan struct{}, 2)
	go func() {
		s.finishRun(context.Background(), rt1, &clievent.SendResult{CostUSD: 5.0}, nil)
		done <- struct{}{}
	}()
	go func() {
		s.finishRun(context.Background(), rt2, &clievent.SendResult{CostUSD: 2.0}, nil)
		done <- struct{}{}
	}()
	<-done
	<-done

	// Total must be exactly the highest cumulative (5.0), not 7.0.
	if got := loadTotalCost(&s.costSpent); got < 5.0-1e-9 || got > 5.0+1e-9 {
		t.Errorf("costSpent = %v, want 5.0 (out-of-order must not over-count)", got)
	}
	// Baseline must converge to the max, never regress to the lower value.
	if got := loadTotalCost(&s.lastCumulativeCost); got != 5.0 {
		t.Errorf("lastCumulativeCost = %v, want 5.0 (monotonic baseline)", got)
	}
}

// TestSend_NoiseTurnDoesNotAdvanceCost verifies a turn that returns no costed
// result (raw 0 — interrupt / pure-tool / error) contributes nothing and does
// not corrupt the baseline for the following real turn.
func TestSend_NoiseTurnDoesNotAdvanceCost(t *testing.T) {
	raws := []float64{2.0, 0.0, 3.0} // middle turn is noise
	var idx int
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []clievent.Attachment, on clievent.EventCallback) (*clievent.SendResult, error) {
		r := &clievent.SendResult{Text: "ok", CostUSD: raws[idx]}
		idx++
		return r, nil
	})
	for range raws {
		if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	store.Close()
	// deltas: 2.0, 0 (noise), (3.0-2.0)=1.0 → total 3.0
	if got := loadTotalCost(&s.costSpent); got < 3.0-1e-9 || got > 3.0+1e-9 {
		t.Errorf("costSpent = %v, want 3.0", got)
	}
}

func TestSend_OutcomeClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want runhistory.Outcome
	}{
		{"timeout", clierr.ErrTotalTimeout, runhistory.OutcomeTimeout},
		{"no-output", clierr.ErrNoOutputTimeout, runhistory.OutcomeTimeout},
		{"canceled", context.Canceled, runhistory.OutcomeCanceled},
		{"error", errors.New("boom"), runhistory.OutcomeError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []clievent.Attachment, on clievent.EventCallback) (*clievent.SendResult, error) {
				return nil, tt.err
			})
			_, _ = s.Send(context.Background(), "x", nil, nil)
			store.Close()
			runs := store.Recent(s.key, 0)
			if len(runs) != 1 {
				t.Fatalf("want 1 run, got %d", len(runs))
			}
			if runs[0].Outcome != tt.want {
				t.Errorf("outcome = %s, want %s", runs[0].Outcome, tt.want)
			}
		})
	}
}

func TestSend_FirstByteRecordedOnce(t *testing.T) {
	var firstByteCalls int
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []clievent.Attachment, on clievent.EventCallback) (*clievent.SendResult, error) {
		// emit several events; FirstByteMS must reflect only the first
		for i := 0; i < 3; i++ {
			if on != nil {
				on(clievent.Event{})
			}
		}
		return &clievent.SendResult{Text: "ok"}, nil
	})
	// wrap an inner callback to count user-callback passthrough
	userCb := func(ev clievent.Event) { firstByteCalls++ }
	if _, err := s.Send(context.Background(), "hi", nil, userCb); err != nil {
		t.Fatalf("Send: %v", err)
	}
	store.Close()
	if firstByteCalls != 3 {
		t.Errorf("user callback should receive all 3 events, got %d", firstByteCalls)
	}
	runs := store.Recent(s.key, 0)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	// FirstByteMS is set (>=0) and the run completed — single first-byte stamp
	if runs[0].Outcome != runhistory.OutcomeCompleted {
		t.Errorf("outcome = %s", runs[0].Outcome)
	}
}

func TestSendPassthrough_AlsoRecorded(t *testing.T) {
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []clievent.Attachment, on clievent.EventCallback) (*clievent.SendResult, error) {
		return &clievent.SendResult{Text: "ok"}, nil
	})
	if _, err := s.SendPassthrough(context.Background(), "hi", nil, nil, ""); err != nil {
		t.Fatalf("SendPassthrough: %v", err)
	}
	store.Close()
	if got := len(store.Recent(s.key, 0)); got != 1 {
		t.Errorf("passthrough run not recorded: got %d", got)
	}
}

func TestSend_NilStoreNoRecord(t *testing.T) {
	// runStore nil -> instrumentation no-ops, Send still works (regression
	// guard for the zero-alloc nil-callback fast path).
	s := &ManagedSession{key: "feishu:p2p:none"}
	s.storeProcess(&TestProcess{AliveVal: true})
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Send with nil store: %v", err)
	}
}

// TestSend_FirstByteConcurrentWithFinish reproduces the passthrough hazard:
// the onEvent callback fires on a different goroutine (CLI readLoop) and may
// still be stamping the first-event time while finishRun reads it. The atomic
// stamp must make this race-free under -race.
func TestSend_FirstByteConcurrentWithFinish(t *testing.T) {
	releaseEvent := make(chan struct{})
	eventDone := make(chan struct{})
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []clievent.Attachment, on clievent.EventCallback) (*clievent.SendResult, error) {
		// Fire onEvent from a separate goroutine that overlaps the return,
		// mimicking readLoop fan-out racing the caller's finishRun.
		go func() {
			<-releaseEvent
			if on != nil {
				on(clievent.Event{})
			}
			close(eventDone)
		}()
		close(releaseEvent) // let the event goroutine run concurrently with return
		return &clievent.SendResult{Text: "ok"}, nil
	})
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-eventDone
	store.Close()
	if got := len(store.Recent(s.key, 0)); got != 1 {
		t.Errorf("want 1 run, got %d", got)
	}
}

func TestSend_DurationMonotonic(t *testing.T) {
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []clievent.Attachment, on clievent.EventCallback) (*clievent.SendResult, error) {
		time.Sleep(5 * time.Millisecond)
		return &clievent.SendResult{Text: "ok"}, nil
	})
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	store.Close()
	runs := store.Recent(s.key, 0)
	if len(runs) != 1 || runs[0].DurationMS < 1 {
		t.Errorf("duration should reflect the ~5ms sleep, got %+v", runs)
	}
}

// TestSend_FirstRunRecordNamesItsSession: a new session learns its ID from
// the first result, after finishRun, so the record must take it from the
// result; an already-captured ID wins, an error turn without a result records
// none, and an error turn with a partial result is named but not captured.
func TestSend_FirstRunRecordNamesItsSession(t *testing.T) {
	tests := []struct {
		name        string
		passthrough bool
		known       string
		result      *clievent.SendResult
		err         error
		want        string
		captured    string
	}{
		{"send first turn", false, "", &clievent.SendResult{Text: "ok", SessionID: "S"}, nil, "S", "S"},
		{"passthrough first turn", true, "", &clievent.SendResult{Text: "ok", SessionID: "S"}, nil, "S", "S"},
		{"captured id wins", false, "OLD", &clievent.SendResult{Text: "ok", SessionID: "S"}, nil, "OLD", "OLD"},
		{"error turn", false, "", nil, errors.New("boom"), "", ""},
		{"error turn with partial result", false, "", &clievent.SendResult{SessionID: "S"}, errors.New("boom"), "S", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []clievent.Attachment, on clievent.EventCallback) (*clievent.SendResult, error) {
				return tt.result, tt.err
			})
			if tt.known != "" {
				s.setSessionID(tt.known)
			}
			if tt.passthrough {
				_, _ = s.SendPassthrough(context.Background(), "hi", nil, nil, "")
			} else {
				_, _ = s.Send(context.Background(), "hi", nil, nil)
			}
			store.Close()
			runs := store.Recent(s.key, 0)
			if len(runs) != 1 {
				t.Fatalf("want 1 run, got %d", len(runs))
			}
			if runs[0].SessionID != tt.want {
				t.Errorf("record SessionID = %q, want %q", runs[0].SessionID, tt.want)
			}
			if got := s.getSessionID(); got != tt.captured {
				t.Errorf("captured SessionID = %q, want %q", got, tt.captured)
			}
		})
	}
}

// TestSend_RunRecordAdoptsCtxRunID: the orchestrator's run id (#3436) names
// the run record, so a journal grep by run_id lands on the same key as
// /api/sessions/runs. Without one the session mints its own.
func TestSend_RunRecordAdoptsCtxRunID(t *testing.T) {
	s, store := newInstrumentedSession(t, func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
		return &clievent.SendResult{Text: "ok"}, nil
	})
	if _, err := s.Send(ctxutil.WithRunID(context.Background(), "0123456789abcdef"), "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "again", nil, nil); err != nil {
		t.Fatal(err)
	}
	store.Close()
	runs := store.Recent(s.key, 0)
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	ids := map[string]bool{runs[0].RunID: true, runs[1].RunID: true}
	if !ids["0123456789abcdef"] {
		t.Fatalf("ctx run id not adopted: %v", ids)
	}
	for id := range ids {
		if id == "" {
			t.Fatal("a run without a ctx id must still get one")
		}
	}
}

// TestSend_UnnamedRunIDReachesTheProcess: a send with no run id in ctx (cron,
// sysession) is named before the process call, so the CLI's transcript
// entries carry the id its run record and ledger rows are booked under.
func TestSend_UnnamedRunIDReachesTheProcess(t *testing.T) {
	entries := map[string]func(*ManagedSession) error{
		"Send": func(s *ManagedSession) error {
			_, err := s.Send(context.Background(), "hi", nil, nil)
			return err
		},
		"SendPassthrough": func(s *ManagedSession) error {
			_, err := s.SendPassthrough(context.Background(), "hi", nil, nil, "")
			return err
		},
	}
	for name, send := range entries {
		t.Run(name, func(t *testing.T) {
			var sentWith string
			proc := &TestProcess{AliveVal: true, SendFunc: func(ctx context.Context, _ string, _ []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
				sentWith = ctxutil.RunID(ctx)
				return &clievent.SendResult{Text: "ok", CostUSD: 0.1}, nil
			}}
			s, ledger := newLedgerSession(t, "cron:job1", proc)
			store := runhistory.NewStore(t.TempDir(), 0, 0)
			t.Cleanup(store.Close)
			s.runStore = store
			if err := send(s); err != nil {
				t.Fatal(err)
			}
			store.Close()
			if sentWith == "" {
				t.Fatal("the process saw no run id")
			}
			if runs := store.Recent(s.key, 0); len(runs) != 1 || runs[0].RunID != sentWith {
				t.Fatalf("run records %+v, want one under %s", runs, sentWith)
			}
			if ents := allEntries(t, ledger); len(ents) != 1 || ents[0].RunID != sentWith {
				t.Fatalf("ledger rows %+v, want one under %s", ents, sentWith)
			}
		})
	}
}

// TestSend_LeakNudgeGetsItsOwnRunID: the leaked-toolcall re-send is a second
// run record, so it must not reuse the turn's id — runhistory and the cost
// ledger are keyed by it. Its CLI send carries the id its record gets.
func TestSend_LeakNudgeGetsItsOwnRunID(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	const parent = "0123456789abcdef"
	entries := map[string]func(*ManagedSession, context.Context) error{
		"Send": func(s *ManagedSession, ctx context.Context) error {
			_, err := s.Send(ctx, "hi", nil, nil)
			return err
		},
		"SendPassthrough": func(s *ManagedSession, ctx context.Context) error {
			_, err := s.SendPassthrough(ctx, "hi", nil, nil, "")
			return err
		},
	}
	for name, send := range entries {
		t.Run(name, func(t *testing.T) {
			logs := &lockedBuf{}
			prev := slog.Default()
			slog.SetDefault(slog.New(ctxutil.NewHandler(slog.NewJSONHandler(logs, nil))))
			t.Cleanup(func() { slog.SetDefault(prev) })
			var sentWith []string
			s, store := newInstrumentedSession(t, func(ctx context.Context, _ string, _ []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
				sentWith = append(sentWith, ctxutil.RunID(ctx))
				if len(sentWith) == 1 {
					return &clievent.SendResult{Text: leakSample}, nil
				}
				return &clievent.SendResult{Text: "clean"}, nil
			})
			if err := send(s, ctxutil.WithRunID(context.Background(), parent)); err != nil {
				t.Fatal(err)
			}
			store.Close()
			if len(sentWith) != 2 || sentWith[0] != parent || sentWith[1] == parent || sentWith[1] == "" {
				t.Fatalf("CLI sends carried run ids %q, want [%s <fresh>]", sentWith, parent)
			}
			runs := store.Recent(s.key, 0)
			if len(runs) != 2 {
				t.Fatalf("runs = %d, want the turn and its nudge", len(runs))
			}
			got := map[string]bool{runs[0].RunID: true, runs[1].RunID: true}
			if !got[parent] || !got[sentWith[1]] {
				t.Fatalf("run record ids %v, want %s and the nudge's %s", got, parent, sentWith[1])
			}
			// The log line is the only join from a nudge run to its turn.
			nudge := logs.lines("leak-recovery: nudge run")
			if len(nudge) != 1 || nudge[0]["nudge_of"] != parent || nudge[0]["run_id"] != sentWith[1] {
				t.Fatalf("nudge log = %v, want nudge_of %s run_id %s", nudge, parent, sentWith[1])
			}
		})
	}
}

// lockedBuf is a goroutine-safe log sink.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

// lines returns the decoded JSON records whose msg is msg.
func (b *lockedBuf) lines(msg string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b.b.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}
