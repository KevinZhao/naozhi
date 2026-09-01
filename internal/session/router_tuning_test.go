package session

// router_tuning_test.go — SetSessionTuning's §4.4 apply matrix, F9 path
// selection, and the §6 R8 ack-before-persist rule.
// docs/rfc/dashboard-model-effort-control.md §5 (F9 路径选择 / 校验 rows).

import (
	"context"
	"errors"
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
	}
	r.bkStore.defaultBackend = "claude"
	r.bkStore.backendOverrides = make(map[string]string)
	r.bkStore.backendEfforts = map[string]string{}
	r.wsStore.overrides = make(map[string]string)
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
	if _, err := r.SetSessionTuning(ctx, "missing", strp("opus"), nil); !errors.Is(err, ErrTuningUnknownSession) {
		t.Errorf("unknown key: err = %v, want ErrTuningUnknownSession", err)
	}
	// claude has no EffortTier capability → setting a tier is a 400, not a
	// silent record (§4.3).
	if _, err := r.SetSessionTuning(ctx, "k1", nil, strp("high")); !errors.Is(err, ErrTuningEffortUnsupported) {
		t.Errorf("claude+effort: err = %v, want ErrTuningEffortUnsupported", err)
	}
	// …but CLEARING an effort on claude is fine (no-op, not an error).
	if _, err := r.SetSessionTuning(ctx, "k1", nil, strp("")); err != nil {
		t.Errorf("clearing effort on claude must not error: %v", err)
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
// RPC fast path applies; claude always RPC.
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
