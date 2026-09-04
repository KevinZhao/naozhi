package session

// router_tuning_test.go — SetSessionTuning's §4.4 apply matrix, F9 path
// selection, and the §6 R8 ack-before-persist rule.
// docs/rfc/dashboard-model-effort-control.md §5 (F9 路径选择 / 校验 rows).

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// tuningFakeProc extends TestProcess with a controllable SetModel so tests
// can drive the RPC path (success / rejection) without a real CLI.
type tuningFakeProc struct {
	*TestProcess
	setModelErr    error
	setModelCalled bool
	setModelArg    string
}

func (p *tuningFakeProc) SetModel(_ context.Context, model string) error {
	p.setModelCalled = true
	p.setModelArg = model
	return p.setModelErr
}

func strp(s string) *string { return &s }

func mkTuningTestRouter(t *testing.T) *Router {
	t.Helper()
	r := &Router{
		ss:         sessionStore{sessions: make(map[string]*ManagedSession)},
		defaultCWD: "/default/ws",
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{
		"kiro":   cli.NewWrapper("/bin/false", &cli.ACPProtocol{BackendID: "kiro"}, "kiro"),
		"claude": cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "claude"),
		"codex":  cli.NewWrapper("/bin/false", &cli.CodexProtocol{}, "codex"),
	}
	r.bkStore.defaultBackend = "claude"
	r.bkStore.backendOverrides = make(map[string]string)
	r.bkStore.backendEfforts = map[string]string{}
	return r
}

func addTuningSession(r *Router, key, backend string, proc processIface) *ManagedSession {
	s := newSessionWithID(key, "sess-"+key)
	s.SetBackend(backend)
	if proc != nil {
		s.storeProcess(proc)
	}
	r.ss.sessions[key] = s
	return s
}

func TestSetSessionTuning_Validation(t *testing.T) {
	r := mkTuningTestRouter(t)
	addTuningSession(r, "k1", "claude", nil)
	ctx := context.Background()

	if _, err := r.SetSessionTuning(ctx, "k1", nil, nil); err == nil {
		t.Error("no fields provided must error")
	}
	if _, err := r.SetSessionTuning(ctx, "k1", strp("-inject"), nil); err == nil {
		t.Error("flag-shaped model must be rejected (tuningspec)")
	}
	if _, err := r.SetSessionTuning(ctx, "k1", nil, strp("ultra")); err == nil {
		t.Error("out-of-set effort must be rejected")
	}
	// A key with no session is a not-yet-spawned dashboard session: the pick
	// is parked (deferred), never 404 — see TestSetSessionTuning_PendingSession.
	if via, err := r.SetSessionTuning(ctx, "missing", strp("opus"), nil); err != nil || via != TuningAppliedDeferred {
		t.Errorf("unknown key: via=%q err=%v, want deferred/nil", via, err)
	}
	// The observed-model manifest serves back whatever claude reported in
	// its init frame, including the context-window suffix. A popover pick of
	// that id must round-trip (suspended → deferred), not 400 — this was the
	// "every model chip click fails" regression once the deployment moved to
	// `…[1m]` model ids.
	if via, err := r.SetSessionTuning(ctx, "k1", strp("us.anthropic.claude-fable-5-1[1m]"), nil); err != nil || via != TuningAppliedDeferred {
		t.Errorf("[1m] model: via=%q err=%v, want deferred/nil", via, err)
	}
	if got := r.ss.sessions["k1"].TuningModel(); got != "us.anthropic.claude-fable-5-1[1m]" {
		t.Errorf("[1m] model: recorded = %q", got)
	}
	if _, err := r.SetSessionTuning(ctx, "k1", strp("[1m]"), nil); err == nil {
		t.Error("leading bracket must still be rejected (leading-char gate)")
	}
	if _, err := r.SetSessionTuning(ctx, "k1", strp(""), nil); err != nil {
		t.Errorf("clearing model must not error: %v", err)
	}
	// claude accepts `--effort` since CLI 2.1.226 (#2412: EffortTier=true),
	// so a tier on a claude session is recorded (suspended → deferred), not
	// rejected. Pinning this here is what catches a future Capabilities()
	// regression at the session layer, not just in internal/cli.
	if via, err := r.SetSessionTuning(ctx, "k1", nil, strp("high")); err != nil || via != TuningAppliedDeferred {
		t.Errorf("claude+effort: via=%q err=%v, want deferred/nil", via, err)
	}
	if got := r.ss.sessions["k1"].TuningEffort(); got != "high" {
		t.Errorf("claude+effort: recorded tier = %q, want high", got)
	}
	// Clearing it is fine too.
	if _, err := r.SetSessionTuning(ctx, "k1", nil, strp("")); err != nil {
		t.Errorf("clearing effort on claude must not error: %v", err)
	}
	// codex has no EffortTier capability (its knob is -c
	// model_reasoning_effort=) → setting a tier is a 400, not a silent
	// record (§4.3).
	addTuningSession(r, "k-codex", "codex", nil)
	if _, err := r.SetSessionTuning(ctx, "k-codex", nil, strp("high")); !errors.Is(err, ErrTuningEffortUnsupported) {
		t.Errorf("codex+effort: err = %v, want ErrTuningEffortUnsupported", err)
	}
}

// TestSetSessionTuning_RPCPath covers §6 R8: record only after ack success;
// a CLI rejection leaves no override behind.
func TestSetSessionTuning_RPCPath(t *testing.T) {
	ctx := context.Background()

	t.Run("success records override and mirrors display model", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		proc := &tuningFakeProc{TestProcess: NewTestProcess()}
		s := addTuningSession(r, "k1", "claude", proc)

		mode, err := r.SetSessionTuning(ctx, "k1", strp("opus"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if mode != TuningAppliedRPC {
			t.Errorf("mode = %q, want rpc", mode)
		}
		if !proc.setModelCalled || proc.setModelArg != "opus" {
			t.Errorf("SetModel(proc) not invoked correctly: called=%v arg=%q", proc.setModelCalled, proc.setModelArg)
		}
		if s.TuningModel() != "opus" {
			t.Errorf("TuningModel = %q, want opus", s.TuningModel())
		}
		if s.Model() != "opus" {
			t.Errorf("display Model = %q, want opus (F11 mirror)", s.Model())
		}
	})

	t.Run("rejection records nothing and surfaces CLI text", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		proc := &tuningFakeProc{TestProcess: NewTestProcess()}
		proc.setModelErr = cli.ErrSetModelRejected
		s := addTuningSession(r, "k1", "claude", proc)

		_, err := r.SetSessionTuning(ctx, "k1", strp("haiku"), nil)
		if !errors.Is(err, cli.ErrSetModelRejected) {
			t.Fatalf("err = %v, want ErrSetModelRejected", err)
		}
		if s.TuningModel() != "" {
			t.Errorf("rejected override was recorded: %q (violates §6 R8)", s.TuningModel())
		}
		if proc.AliveVal != true {
			t.Error("rejection must not touch the process")
		}
	})

	t.Run("transport failure degrades to deferred record", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		proc := &tuningFakeProc{TestProcess: NewTestProcess()}
		proc.setModelErr = errors.New("broken pipe")
		s := addTuningSession(r, "k1", "claude", proc)

		mode, err := r.SetSessionTuning(ctx, "k1", strp("opus"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if mode != TuningAppliedDeferred {
			t.Errorf("mode = %q, want deferred", mode)
		}
		if s.TuningModel() != "opus" {
			t.Errorf("override must be recorded for next spawn, got %q", s.TuningModel())
		}
	})

	t.Run("fake without SetModel degrades to deferred", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		s := addTuningSession(r, "k1", "claude", NewTestProcess()) // plain fake: no SetModel method
		mode, err := r.SetSessionTuning(ctx, "k1", strp("opus"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if mode != TuningAppliedDeferred {
			t.Errorf("mode = %q, want deferred", mode)
		}
		if s.TuningModel() != "opus" {
			t.Errorf("override not recorded: %q", s.TuningModel())
		}
	})
}

// TestSetSessionTuning_F9PathSelection is the §5 "F9 路径选择" row: a kiro
// session with an effective effort tier must take the respawn path for a
// model switch (RPC would silently drop the tier, F9); without a tier the
// RPC fast path applies; claude without an effective tier takes RPC too
// (claude's EffortTier is true since #2412, so a claude session WITH a
// configured tier respawns on model switch just like kiro — F9 is applied
// conservatively there because whether claude's set_model preserves the
// launch-time --effort pin has not been measured).
func TestSetSessionTuning_F9PathSelection(t *testing.T) {
	ctx := context.Background()

	t.Run("kiro with tuning effort: model switch respawns", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		proc := &tuningFakeProc{TestProcess: NewTestProcess()}
		s := addTuningSession(r, "k1", "kiro", proc)
		s.SetTuningEffort("low")

		mode, err := r.SetSessionTuning(ctx, "k1", strp("claude-sonnet-4.6"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if mode != TuningAppliedRespawn {
			t.Errorf("mode = %q, want respawn (F9: RPC would reset the tier)", mode)
		}
		if proc.setModelCalled {
			t.Error("RPC must not fire on the respawn path")
		}
		if proc.AliveVal {
			t.Error("process must be closed for lazy respawn")
		}
		if s.TuningModel() != "claude-sonnet-4.6" {
			t.Errorf("override not recorded before close: %q", s.TuningModel())
		}
	})

	t.Run("kiro with backend-level effort: model switch respawns", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		r.bkStore.backendEfforts = map[string]string{"kiro": "high"}
		proc := &tuningFakeProc{TestProcess: NewTestProcess()}
		addTuningSession(r, "k1", "kiro", proc)

		mode, err := r.SetSessionTuning(ctx, "k1", strp("claude-sonnet-4.6"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if mode != TuningAppliedRespawn {
			t.Errorf("mode = %q, want respawn (backend tier counts as effective effort)", mode)
		}
	})

	t.Run("kiro without any effort: model switch takes RPC", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		proc := &tuningFakeProc{TestProcess: NewTestProcess()}
		addTuningSession(r, "k1", "kiro", proc)

		mode, err := r.SetSessionTuning(ctx, "k1", strp("claude-sonnet-4.6"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if mode != TuningAppliedRPC {
			t.Errorf("mode = %q, want rpc (no tier to protect)", mode)
		}
		if !proc.setModelCalled {
			t.Error("RPC path must invoke SetModel")
		}
		if proc.AliveVal != true {
			t.Error("RPC path must not restart the process")
		}
	})

	t.Run("effort change on live kiro respawns", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		proc := &tuningFakeProc{TestProcess: NewTestProcess()}
		s := addTuningSession(r, "k1", "kiro", proc)

		mode, err := r.SetSessionTuning(ctx, "k1", nil, strp("max"))
		if err != nil {
			t.Fatal(err)
		}
		if mode != TuningAppliedRespawn {
			t.Errorf("mode = %q, want respawn (effort has no runtime channel, F4)", mode)
		}
		if s.TuningEffort() != "max" {
			t.Errorf("TuningEffort = %q, want max", s.TuningEffort())
		}
		if proc.AliveVal {
			t.Error("process must be closed")
		}
	})

	t.Run("suspended session records only", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		s := addTuningSession(r, "k1", "kiro", nil) // no process
		mode, err := r.SetSessionTuning(ctx, "k1", strp("claude-haiku-4.5"), strp("low"))
		if err != nil {
			t.Fatal(err)
		}
		if mode != TuningAppliedDeferred {
			t.Errorf("mode = %q, want deferred", mode)
		}
		if s.TuningModel() != "claude-haiku-4.5" || s.TuningEffort() != "low" {
			t.Errorf("overrides not recorded: model=%q effort=%q", s.TuningModel(), s.TuningEffort())
		}
	})

	t.Run("clear via empty strings", func(t *testing.T) {
		r := mkTuningTestRouter(t)
		s := addTuningSession(r, "k1", "kiro", nil)
		s.SetTuningModel("m1")
		s.SetTuningEffort("low")
		if _, err := r.SetSessionTuning(ctx, "k1", strp(""), strp("")); err != nil {
			t.Fatal(err)
		}
		if s.TuningModel() != "" || s.TuningEffort() != "" {
			t.Errorf("clear failed: model=%q effort=%q", s.TuningModel(), s.TuningEffort())
		}
	})
}

// TestSetSessionTuning_ErrorTextIsSanitizedUpstream documents where trust
// boundaries sit: the CLI rejection text reaching the API error was already
// sanitized at the protocol layer (parseControlAck / ACP interception), so
// this layer passes it through verbatim.
func TestSetSessionTuning_ErrorTextIsSanitizedUpstream(t *testing.T) {
	r := mkTuningTestRouter(t)
	proc := &tuningFakeProc{TestProcess: NewTestProcess()}
	proc.setModelErr = errors.New("set_model rejected by CLI: policy says no")
	// Wrap so errors.Is matches the sentinel like the real path does.
	proc.setModelErr = errors.Join(cli.ErrSetModelRejected, proc.setModelErr)
	addTuningSession(r, "k1", "claude", proc)

	_, err := r.SetSessionTuning(context.Background(), "k1", strp("haiku"), nil)
	if err == nil || !strings.Contains(err.Error(), "policy says no") {
		t.Errorf("CLI text must reach the caller for the dashboard toast, got: %v", err)
	}
}

// TestSetSessionTuning_ClearModelNeverTakesRPC pins the "恢复默认" fix: a
// pointer-to-"" model means "drop the override, let the config chain decide
// on the next spawn". There is no model id to hand the CLI, so the RPC fast
// path must never fire — sending set_model("") clears kiro's header and
// makes claude return an error, leaving a live session that can never be
// restored to default. Clearing behaves like an effort change: lazy respawn
// when alive, record-only when suspended.
func TestSetSessionTuning_ClearModelNeverTakesRPC(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		backend  string
		effort   string // pre-existing tuning effort (kiro only)
		alive    bool
		wantMode string
	}{
		{"claude alive", "claude", "", true, TuningAppliedRespawn},
		{"kiro alive, no effort tier", "kiro", "", true, TuningAppliedRespawn},
		{"kiro alive, with effort tier", "kiro", "low", true, TuningAppliedRespawn},
		{"claude suspended", "claude", "", false, TuningAppliedDeferred},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mkTuningTestRouter(t)
			var proc *tuningFakeProc
			var s *ManagedSession
			if tc.alive {
				proc = &tuningFakeProc{TestProcess: NewTestProcess()}
				s = addTuningSession(r, "k1", tc.backend, proc)
			} else {
				s = addTuningSession(r, "k1", tc.backend, nil)
			}
			s.SetTuningModel("m-old")
			if tc.effort != "" {
				s.SetTuningEffort(tc.effort)
			}

			mode, err := r.SetSessionTuning(ctx, "k1", strp(""), nil)
			if err != nil {
				t.Fatalf("clearing model must not error: %v", err)
			}
			if mode == TuningAppliedRPC {
				t.Fatal("applied_via = rpc — empty model must never be sent over the control channel")
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if proc != nil {
				if proc.setModelCalled {
					t.Errorf("SetModel(%q) was invoked on the live process", proc.setModelArg)
				}
				if proc.AliveVal {
					t.Error("live process must be closed for lazy respawn so the next spawn drops --model")
				}
			}
			if s.TuningModel() != "" {
				t.Errorf("TuningModel = %q, want cleared", s.TuningModel())
			}
		})
	}
}

// TestInstallFreshSessionLocked_InheritsTuning pins the respawn half of the
// tuning lifecycle: SetSessionTuning's respawn/deferred paths only record
// the override on the CURRENT ManagedSession, and the next spawn builds its
// argv from it (resolveSpawnParamsLocked) — but installFreshSessionLocked
// then REPLACES that struct. If the fresh struct does not carry the override
// forward, sessions.json is rewritten without it, the following TTL recycle
// spawns back on the config default, and a naozhi restart in between reads
// the surviving shim as arg-drift and rebuilds it as default. The user label
// is operator-owned state of the same shape and rides along.
func TestInstallFreshSessionLocked_InheritsTuning(t *testing.T) {
	r := NewRouter(RouterConfig{})
	wrapper := cli.NewWrapper("/bin/false", &cli.ACPProtocol{BackendID: "kiro"}, "kiro")
	const key = "dash:direct:inherit:general"

	old := newSessionWithID(key, "sess-old")
	old.SetBackend("kiro")
	old.SetTuningModel("claude-haiku-4.5")
	old.SetTuningEffort("low")
	old.SetUserLabel("my label")
	old.setLabelOrigin("auto")
	r.mu.Lock()
	r.ss.sessions[key] = old
	r.indexAdd(key)

	// Mirror spawnSession: snapshot under the first r.mu hold, then install.
	// The fresh entry must be fed from that snapshot, not from a re-read of
	// the map — so swap the map entry for an unrelated stub in between (what
	// RegisterForResume / Remove can do during the unlocked history copy) and
	// require the ORIGINAL values to win.
	_, _, _, _, ov := snapshotOldSessionLocked(old)
	stub := newSessionWithID(key, "sess-stub")
	stub.SetTuningModel("stub-model")
	stub.SetUserLabel("stub label")
	r.ss.sessions[key] = stub

	fresh := r.installFreshSessionLocked(
		key, &cli.Process{}, "/ws", "kiro", "", wrapper, "sess-old",
		nil, nil, 0, 0, 0, false, "sess-old", 0, ov,
	)
	r.mu.Unlock()

	if fresh == old {
		t.Fatal("test premise broken: installFreshSessionLocked must allocate a new struct")
	}
	if got := fresh.TuningModel(); got != "claude-haiku-4.5" {
		t.Errorf("TuningModel after respawn = %q, want claude-haiku-4.5", got)
	}
	if got := fresh.TuningEffort(); got != "low" {
		t.Errorf("TuningEffort after respawn = %q, want low", got)
	}
	if got := fresh.UserLabel(); got != "my label" {
		t.Errorf("UserLabel after respawn = %q, want %q", got, "my label")
	}
	if got := fresh.LabelOrigin(); got != "auto" {
		t.Errorf("LabelOrigin after respawn = %q, want auto (AutoTitler must keep ownership)", got)
	}
	if r.SessionFor(key) != fresh {
		t.Error("fresh session not published under key")
	}
	if fresh.TuningModel() == "stub-model" || fresh.UserLabel() == "stub label" {
		t.Error("fresh entry took values from a re-read of r.ss.sessions[key] instead of the snapshot")
	}
}

// TestRenameSession_InheritsTuning covers the takeover/rename path, which
// builds its own fresh struct instead of going through
// installFreshSessionLocked and must carry the override too.
func TestRenameSession_InheritsTuning(t *testing.T) {
	r := NewRouter(RouterConfig{})
	const oldKey = "scratch:abc:general:general"
	const newKey = "feishu:direct:alice:aside-general-deadbeef"

	s := newSessionWithID(oldKey, "sess-rn")
	s.SetTuningModel("claude-haiku-4.5")
	s.SetTuningEffort("low")
	s.SetUserLabel("auto title")
	s.setLabelOrigin("auto")
	r.mu.Lock()
	r.ss.sessions[oldKey] = s
	r.indexAdd(oldKey)
	r.mu.Unlock()

	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("RenameSession returned false")
	}
	got := r.SessionFor(newKey)
	if got == nil {
		t.Fatal("renamed session missing")
	}
	if got.TuningModel() != "claude-haiku-4.5" || got.TuningEffort() != "low" {
		t.Errorf("tuning lost across rename: model=%q effort=%q", got.TuningModel(), got.TuningEffort())
	}
	if got.UserLabel() != "auto title" || got.LabelOrigin() != "auto" {
		t.Errorf("label+origin must travel as one unit across rename: label=%q origin=%q",
			got.UserLabel(), got.LabelOrigin())
	}
}

// TestSetSessionTuning_RespawnReleasesActiveSlot pins the capacity
// bookkeeping of the lazy-respawn path: closing the live process frees a
// session slot exactly like a TTL recycle / evict does, so activeCount must
// drop. Without it every tuning respawn leaks one slot (the follow-up spawn
// Adds 1 again) until the max-sessions gate starts evicting healthy sessions.
func TestSetSessionTuning_RespawnReleasesActiveSlot(t *testing.T) {
	r := mkTuningTestRouter(t)
	proc := &tuningFakeProc{TestProcess: NewTestProcess()}
	addTuningSession(r, "k1", "kiro", proc)
	r.ss.activeCount.Store(1) // the session above is the one live, non-exempt entry

	mode, err := r.SetSessionTuning(context.Background(), "k1", nil, strp("max"))
	if err != nil {
		t.Fatal(err)
	}
	if mode != TuningAppliedRespawn {
		t.Fatalf("mode = %q, want respawn (test premise)", mode)
	}
	if got := r.ss.activeCount.Load(); got != 0 {
		t.Errorf("activeCount after tuning respawn = %d, want 0 (slot leaked)", got)
	}
}

// TestSetSessionTuning_PendingSession covers the pre-spawn path: a dashboard
// session exists only client-side until its first message, but its header
// chips are already clickable. The pick must be recorded server-side, shape
// the FIRST spawn's argv (resolveSpawnParamsLocked), land on the fresh
// ManagedSession (spawnSession's consume step) and then leave no one-shot
// residue. Clearing both fields before spawn deletes the record outright.
func TestSetSessionTuning_PendingSession(t *testing.T) {
	r := mkTuningTestRouter(t)
	ctx := context.Background()
	const key = "dashboard:direct:2026-09-04-1-naozhi:general"

	via, err := r.SetSessionTuning(ctx, key, strp("us.anthropic.claude-opus-5[1m]"), nil)
	if err != nil || via != TuningAppliedDeferred {
		t.Fatalf("model on pending key: via=%q err=%v, want deferred/nil", via, err)
	}
	if via, err := r.SetSessionTuning(ctx, key, nil, strp("high")); err != nil || via != TuningAppliedDeferred {
		t.Fatalf("effort on pending key: via=%q err=%v, want deferred/nil", via, err)
	}
	if _, ok := r.ss.sessions[key]; ok {
		t.Fatal("recording a pending pick must not create a ManagedSession")
	}
	if got := r.bkStore.tuningOverrides[key]; got != (pendingTuning{Model: "us.anthropic.claude-opus-5[1m]", Effort: "high"}) {
		t.Fatalf("pending record = %+v", got)
	}

	// Validation still applies before anything is parked.
	if _, err := r.SetSessionTuning(ctx, key, strp("-inject"), nil); err == nil {
		t.Error("flag-shaped model on pending key must be rejected")
	}
	if _, err := r.SetSessionTuning(ctx, key, nil, strp("ultra")); err == nil {
		t.Error("out-of-set effort on pending key must be rejected")
	}

	// First spawn's argv reads the parked pick (tuning is the top of both chains).
	r.mu.Lock()
	sp := r.resolveSpawnParamsLocked(key, "", AgentOpts{Backend: "claude", Workspace: "/ws"})
	r.mu.Unlock()
	if sp.Model != "us.anthropic.claude-opus-5[1m]" || sp.Effort != "high" {
		t.Errorf("first spawn params: model=%q effort=%q, want the parked pick", sp.Model, sp.Effort)
	}
	// resolve is a read: a failed Spawn() must leave the pick for the retry.
	if _, ok := r.bkStore.tuningOverrides[key]; !ok {
		t.Error("resolveSpawnParamsLocked must not consume the pending pick")
	}

	// spawnSession's consume step moves it onto the fresh entry and drops it.
	r.mu.Lock()
	ov := r.consumePendingTuningLocked(key, sessionOverrides{userLabel: "keep"})
	_, stillThere := r.bkStore.tuningOverrides[key]
	r.mu.Unlock()
	if ov.tuningModel != "us.anthropic.claude-opus-5[1m]" || ov.tuningEffort != "high" || ov.userLabel != "keep" {
		t.Errorf("consumed overrides = %+v", ov)
	}
	if stillThere {
		t.Error("consume must delete the one-shot record")
	}
	r.mu.Lock()
	if ov2 := r.consumePendingTuningLocked(key, sessionOverrides{}); ov2 != (sessionOverrides{}) {
		t.Errorf("second consume must be a no-op, got %+v", ov2)
	}
	r.mu.Unlock()

	// 恢复默认 on both fields before any spawn leaves no residue.
	if _, err := r.SetSessionTuning(ctx, key, strp("sonnet"), strp("low")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetSessionTuning(ctx, key, strp(""), strp("")); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.bkStore.tuningOverrides[key]; ok {
		t.Error("clearing both fields must delete the pending record")
	}
}

// TestSetSessionTuning_PendingCapacity pins the DoS bound on the pre-spawn
// store: past maxTuningOverrides a NEW key is refused with ErrTuningCapacity
// (nothing recorded), while updating an existing key still succeeds.
func TestSetSessionTuning_PendingCapacity(t *testing.T) {
	r := mkTuningTestRouter(t)
	ctx := context.Background()
	r.bkStore.tuningOverrides = make(map[string]pendingTuning, maxTuningOverrides)
	for i := 0; i < maxTuningOverrides; i++ {
		r.bkStore.tuningOverrides["k:"+strings.Repeat("x", 3)+string(rune('a'+i%26))+strconv.Itoa(i)] = pendingTuning{Model: "m"}
	}
	if _, err := r.SetSessionTuning(ctx, "dashboard:direct:new:general", strp("opus"), nil); !errors.Is(err, ErrTuningCapacity) {
		t.Errorf("new key at cap: err = %v, want ErrTuningCapacity", err)
	}
	var existing string
	for k := range r.bkStore.tuningOverrides {
		existing = k
		break
	}
	if _, err := r.SetSessionTuning(ctx, existing, strp("opus"), nil); err != nil {
		t.Errorf("updating an existing key at cap must succeed: %v", err)
	}
	if got := r.bkStore.tuningOverrides[existing].Model; got != "opus" {
		t.Errorf("existing key model = %q, want opus", got)
	}
}
