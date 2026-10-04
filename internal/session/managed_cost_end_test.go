package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const endSID = "2420ea6d-c992-4327-90f7-a0c7a992867e"

// endFixture is a router whose claude transcripts live in a temp claudeDir,
// with a ledger that has learned one rate per model: 1e-6 USD per weighted
// token (fallback weights), so an estimate is weighted(tokens)·1e-6.
type endFixture struct {
	r         *Router
	ledger    *costledger.Store
	claudeDir string
	ws        string
	t0        time.Time
}

func newEndFixture(t *testing.T, owned func(string) bool) *endFixture {
	t.Helper()
	f := &endFixture{claudeDir: t.TempDir(), ws: t.TempDir(), t0: time.Now().Add(-time.Hour).Truncate(time.Millisecond)}
	f.r = NewRouter(RouterConfig{Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude"), ClaudeDir: f.claudeDir})
	t.Cleanup(f.r.Shutdown)
	f.ledger = costledger.NewStore(t.TempDir(), costledger.Options{})
	t.Cleanup(f.ledger.Close)
	f.r.runs.cost = newCostAccounting(f.ledger, owned)
	for _, m := range []string{"claude-opus-5-5", "claude-haiku-4-5"} {
		f.ledger.Rates().Observe(costledger.ModelDelta{Model: m, CostUSD: 5e-3, Tokens: costledger.Tokens{Output: 1000}})
	}
	return f
}

// line is one assistant line sec seconds after t0.
func (f *endFixture) line(sec int, id, model string, out int64) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": f.t0.Add(time.Duration(sec) * time.Second).Format(time.RFC3339Nano), "sessionId": endSID,
		"message": map[string]any{"id": id, "model": model, "usage": map[string]any{"input_tokens": 0, "output_tokens": out}},
	})
	return string(b) + "\n"
}

func appendFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	for _, l := range lines {
		if _, err := fh.WriteString(l); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *endFixture) mainPath() string { return claudefs.SessionJSONL(f.claudeDir, f.ws, endSID) }

func (f *endFixture) workflowPath() string {
	return filepath.Join(claudefs.SubagentsDir(claudefs.ProjectDir(f.claudeDir, f.ws), endSID), "workflows", "wf_1", "agent-a1.jsonl")
}

// end is a non-detached end of endSID whose last result came at t0+10s.
func (f *endFixture) end() *cli.ProcessEnd {
	return &cli.ProcessEnd{StartedAt: f.t0, LastResultAt: f.t0.Add(10 * time.Second), EndedAt: f.t0.Add(time.Minute), SessionID: endSID,
		Shadow: clievent.ShadowUsage{Models: []clievent.ShadowModel{{Model: "claude-opus-5-5", Output: 1}}}}
}

// endingProcess is a TestProcess that delivers end to its SetOnEnd hook,
// once, when killed or closed, as a real process's read loop exits once.
type endingProcess struct {
	*TestProcess
	end   *cli.ProcessEnd
	mu    sync.Mutex
	onEnd func(cli.ProcessEnd)
	ended bool
}

func newEndingProcess(state cli.ProcessState, end *cli.ProcessEnd) *endingProcess {
	p := NewTestProcess()
	p.StateVal = state
	return &endingProcess{TestProcess: p, end: end}
}

func (p *endingProcess) SetOnEnd(fn func(cli.ProcessEnd)) {
	p.mu.Lock()
	p.onEnd = fn
	p.mu.Unlock()
}

func (p *endingProcess) Kill()  { p.TestProcess.Kill(); p.deliver() }
func (p *endingProcess) Close() { p.TestProcess.Close(); p.deliver() }

func (p *endingProcess) deliver() {
	p.mu.Lock()
	fn, now := p.onEnd, p.onEnd != nil && !p.ended
	p.ended = p.ended || now
	p.mu.Unlock()
	if now {
		fn(*p.end)
	}
}

// inject installs key's session on proc, bound as the spawn path binds it.
func (f *endFixture) inject(key string, proc *endingProcess) *ManagedSession {
	s := injectSession(f.r, key, proc)
	s.costAcct = f.r.runs.cost
	s.SetBackend("claude")
	s.setWorkspace(f.ws)
	s.setSessionID(endSID)
	bookProcessEnd(s, proc, f.claudeDir)
	return s
}

func (f *endFixture) entries(t *testing.T) []costledger.Entry {
	t.Helper()
	if !f.r.runs.cost.waitEnds(5 * time.Second) {
		t.Fatal("process-end booking did not finish")
	}
	return allEntries(t, f.ledger)
}

// The loss path #3210 is about: a turn no Send owns (here a workflow the CLI
// runs in the background) is killed by stuck_running. Its spend after the
// last result — the main loop's and the workflow agent's lines — is booked
// at the learned rates; the lines the last result already reported are not,
// and the transcripts win over the main-loop-only shadow account.
func TestProcessEnd_StuckRunningKillBooksTranscriptSpend(t *testing.T) {
	f := newEndFixture(t, nil)
	appendFile(t, f.mainPath(),
		f.line(5, "msg_reported", "claude-opus-5-5", 9000),   // before the last result
		f.line(10, "msg_at_result", "claude-opus-5-5", 9000), // at it: reported too
		f.line(20, "msg_main", "claude-opus-5-5", 100),
		f.line(21, "msg_main", "claude-opus-5-5", 200)) // same message, a later block
	appendFile(t, f.workflowPath(), f.line(30, "msg_wf", "anthropic.claude-haiku-4-5-20251001-v1:0", 400))
	proc := newEndingProcess(cli.StateRunning, f.end())
	const key = "dashboard:direct:stuck:general"
	s := f.inject(key, proc)
	s.lastActive.Store(time.Now().Add(-24 * time.Hour).UnixNano())

	f.r.Cleanup()

	if proc.Alive() || loadAtomicString(&s.deathReason) != "stuck_running" {
		t.Fatalf("alive=%v deathReason=%q, want a stuck_running kill", proc.Alive(), loadAtomicString(&s.deathReason))
	}
	ents := f.entries(t)
	if len(ents) != 1 || ents[0].Kind != costledger.KindPartial || ents[0].SessionKey != key {
		t.Fatalf("entries = %+v, want one partial for %s", ents, key)
	}
	// 200 + 400 output tokens at 5e-6 each (1e-6 per weighted token).
	if want := 600 * 5e-6; !approxEq(ents[0].Amount, want) {
		t.Fatalf("amount = %v, want %v (main 200 + workflow 400 output tokens)", ents[0].Amount, want)
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, ents[0].Amount) {
		t.Fatalf("costSpent = %v, want the partial amount", got)
	}
}

// /new on a session tears its process down with a reset error, not a death
// error; the unfinished turn is booked all the same, from the end.
func TestProcessEnd_ResetBooksTheUnfinishedTurn(t *testing.T) {
	f := newEndFixture(t, nil)
	appendFile(t, f.mainPath(), f.line(20, "msg_main", "claude-opus-5-5", 100))
	proc := newEndingProcess(cli.StateRunning, f.end())
	proc.PassthroughVal = true
	const key = "dashboard:direct:reset:general"
	f.inject(key, proc)

	f.r.Reset(key)

	if ents := f.entries(t); len(ents) != 1 || !approxEq(ents[0].Amount, 100*5e-6) {
		t.Fatalf("entries = %+v, want one partial of the 100 tokens after the last result", ents)
	}
}

// Ends that owe nothing book nothing: a detached CLI, or one whose shim
// outlived the socket, keeps running and reports its own spend; a graceful
// close after an idle result has no lines past it; a cron-owned key is the
// cron run's to account.
func TestProcessEnd_BooksNothingWhenNothingIsOwed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		end   func(f *endFixture) *cli.ProcessEnd
		owned bool
	}{
		{"detached", func(f *endFixture) *cli.ProcessEnd { e := f.end(); e.Detached = true; return e }, false},
		{"shim outlived the socket", func(f *endFixture) *cli.ProcessEnd { e := f.end(); e.ShimLive = true; return e }, false},
		{"idle close after the last result", func(f *endFixture) *cli.ProcessEnd {
			e := f.end()
			e.LastResultAt, e.Shadow = f.t0.Add(25*time.Second), clievent.ShadowUsage{}
			return e
		}, false},
		{"cron-owned", func(f *endFixture) *cli.ProcessEnd { return f.end() }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndFixture(t, func(string) bool { return tc.owned })
			appendFile(t, f.mainPath(), f.line(20, "msg_main", "claude-opus-5-5", 100))
			proc := newEndingProcess(cli.StateReady, tc.end(f))
			f.inject("cron:job-1", proc)
			proc.Close()
			if ents := f.entries(t); len(ents) != 0 {
				t.Fatalf("entries = %+v, want none", ents)
			}
		})
	}
}

// Cron ownership is decided once, when the process ends: a cron run that
// takes the key while the transcripts are read does not drop the booking.
func TestProcessEnd_CronGateDecidedAtTheEnd(t *testing.T) {
	var checks atomic.Int32
	f := newEndFixture(t, func(string) bool { return checks.Add(1) > 1 })
	appendFile(t, f.mainPath(), f.line(20, "msg_main", "claude-opus-5-5", 100))
	proc := newEndingProcess(cli.StateRunning, f.end())
	f.inject("cron:job-1", proc)

	proc.Kill()

	if ents := f.entries(t); len(ents) != 1 || !approxEq(ents[0].Amount, 100*5e-6) {
		t.Fatalf("entries = %+v, want the partial the end found unowned", ents)
	}
}

// Shutdown waits for a booking still running before it closes the ledger,
// so the partial is in the ledger once Close returns.
func TestRunLedgerClose_WaitsForRunningBookings(t *testing.T) {
	f := newEndFixture(t, nil)
	appendFile(t, f.mainPath(), f.line(20, "msg_main", "claude-opus-5-5", 100))
	proc := newEndingProcess(cli.StateRunning, f.end())
	f.inject("dashboard:direct:shutdown:general", proc)
	sem := f.r.runs.cost.endSem
	for range cap(sem) {
		sem <- struct{}{} // hold every slot: the booking queues behind them
	}
	proc.Kill()
	release := time.AfterFunc(100*time.Millisecond, func() {
		for range cap(sem) {
			<-sem
		}
	})
	defer release.Stop()

	f.r.runs.Close()

	if ents := allEntries(t, f.ledger); len(ents) != 1 || !approxEq(ents[0].Amount, 100*5e-6) {
		t.Fatalf("entries after Close = %+v, want the booking that was running", ents)
	}
}

// A resumed spawn marks the main transcript before the process starts and the
// end reads from there, so lines an earlier process wrote are never re-read,
// even ones whose timestamps fall in this process's window. The binding is
// the spawn path's own, and a rename moves it, mark included, to the new key.
func TestProcessEnd_ResumedSpawnReadsPastItsMark(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "spawned", true: "renamed"}[renamed], func(t *testing.T) {
			f := newEndFixture(t, nil)
			appendFile(t, f.mainPath(), f.line(20, "msg_earlier_process", "claude-opus-5-5", 7000))
			const oldKey, newKey = "dashboard:direct:resumed:general", "dashboard:direct:promoted:general"
			dead := &ManagedSession{key: oldKey}
			dead.SetBackend("claude")
			dead.setWorkspace(f.ws)
			dead.setSessionID(endSID)
			putT(f.r, oldKey, dead)
			proc := newEndingProcess(cli.StateReady, f.end())
			f.r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
				if opts.ResumeID != endSID {
					t.Errorf("spawn ResumeID = %q, want a resume of %s", opts.ResumeID, endSID)
				}
				appendFile(t, f.mainPath(), f.line(21, "msg_this_process", "claude-opus-5-5", 100))
				return proc, nil
			}
			if _, _, err := f.r.GetOrCreate(context.Background(), oldKey, AgentOpts{}); err != nil {
				t.Fatal(err)
			}
			key := oldKey
			if renamed {
				if !f.r.RenameSession(oldKey, newKey) {
					t.Fatal("rename failed")
				}
				key = newKey
			}

			proc.Kill()

			ents := f.entries(t)
			if len(ents) != 1 || ents[0].SessionKey != key || !approxEq(ents[0].Amount, 100*5e-6) {
				t.Fatalf("entries = %+v, want one partial of this process's 100 tokens under %s", ents, key)
			}
		})
	}
}

// The production process must offer the hook, or bookProcessEnd is a silent
// no-op and killed turns go unbooked again.
var _ processEndNotifier = (*cli.Process)(nil)

// A shim reattached on restart binds the end too: the CLI writes one more
// assistant frame and dies, and the real process hands that frame's usage
// to the session it was reattached to.
func TestReconnectShims_ReattachedProcessBooksItsEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness and unix sockets")
	}
	dir := shortTempDir(t)
	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
	w.ShimManager = mgr
	r := NewRouter(RouterConfig{Wrapper: w, ClaudeDir: filepath.Join(dir, "claude"), HistoryLoader: &racingHistoryLoader{}})
	t.Cleanup(r.Shutdown)
	ledger := costledger.NewStore(t.TempDir(), costledger.Options{})
	t.Cleanup(ledger.Close)
	r.runs.cost = newCostAccounting(ledger, nil)
	ledger.Rates().Observe(costledger.ModelDelta{Model: "claude-opus-5-5", CostUSD: 5e-3, Tokens: costledger.Tokens{Output: 1000}})

	key := "feishu:direct:alice:general"
	sess := injectSession(r, key, nil)
	sess.costAcct = r.runs.cost
	writeLiveShim(t, dir, r, w, sess, `{"type":"stdout","seq":1,"line":`+
		strconv.Quote(`{"type":"assistant","message":{"id":"msg_1","model":"claude-opus-5-5","usage":{"output_tokens":300}}}`)+"}\n",
		`{"type":"cli_exited","code":1}`+"\n")

	r.ReconnectShimsCtx(context.Background())

	testhelper.Eventually(t, func() bool { return loadTotalCost(&sess.costSpent) > 0 }, 5*time.Second, "reattached process's end not booked")
	ents := settledEntries(t, sess, ledger)
	if len(ents) != 1 || ents[0].SessionKey != key || !approxEq(ents[0].Amount, 300*5e-6) {
		t.Fatalf("entries = %+v, want one partial of the frame's 300 output tokens", ents)
	}
}
