package cron

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// backendCapturingRouter is a minimal SessionRouter that records the
// AgentOpts each GetOrCreate call sees so the propagation contract can be
// asserted without spinning up a real CLI. Returns context.Canceled so
// executeOpt's session-error branch fires fast and we never reach the
// nil-session dereference further down. The Reset / Register* methods are
// no-ops because Sprint 6c only touches the GetOrCreate boundary.
type backendCapturingRouter struct {
	mu       sync.Mutex
	captured []session.AgentOpts
}

func (r *backendCapturingRouter) RegisterCronStub(key, workspace, lastPrompt string) {}
func (r *backendCapturingRouter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string) {
}
func (r *backendCapturingRouter) Reset(key string) {}
func (r *backendCapturingRouter) GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error) {
	r.mu.Lock()
	r.captured = append(r.captured, opts)
	r.mu.Unlock()
	return nil, session.SessionExisting, context.Canceled
}

func (r *backendCapturingRouter) snapshot() []session.AgentOpts {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]session.AgentOpts, len(r.captured))
	copy(out, r.captured)
	return out
}

// TestScheduler_RunJobPropagatesBackendToAgentOpts pins the Sprint 6c
// integration point: when a Job has Backend set, the scheduler MUST pass
// it through to the SessionRouter via AgentOpts.Backend so the router's
// wrapperFor can pick the right CLI wrapper. Without this assertion a
// future refactor splitting executeOpt into helpers could quietly drop
// the field and every cron job would silently snap back to router default.
//
// The test invokes executeOpt directly (not through the cron tick gate)
// so it doesn't need to wait for a real schedule fire. Returning
// context.Canceled from the fake router lets the execute path bail out
// of the post-GetOrCreate work without reaching nil-session paths.
func TestScheduler_RunJobPropagatesBackendToAgentOpts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		jobBackend  string
		wantBackend string
	}{
		{
			name:        "explicit_backend_propagates",
			jobBackend:  "kiro",
			wantBackend: "kiro",
		},
		{
			name:        "empty_backend_leaves_zero_value_for_router_default",
			jobBackend:  "",
			wantBackend: "",
		},
		{
			name:        "alphanumeric_backend_propagates",
			jobBackend:  "claude",
			wantBackend: "claude",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			router := &backendCapturingRouter{}
			s := NewScheduler(SchedulerConfig{
				Router:    router,
				StorePath: t.TempDir() + "/cron.json",
				MaxJobs:   10,
			})
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { s.Stop() })

			j := &Job{
				ID:       "test-backend-prop",
				Schedule: "@every 30m",
				Prompt:   "hello",
				Backend:  tc.jobBackend,
			}
			// Register manually so we don't have to wait for the cron
			// scheduler to tick. executeOpt is the path real ticks land
			// in via cron.AddFunc — calling it directly with viaTriggerNow=true
			// skips jitter so the test stays fast and deterministic.
			s.mu.Lock()
			s.jobs[j.ID] = j
			s.mu.Unlock()

			done := make(chan struct{})
			go func() {
				defer close(done)
				s.executeOpt(j, true)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("executeOpt blocked >2s; should have bailed on context.Canceled")
			}

			captured := router.snapshot()
			if len(captured) != 1 {
				t.Fatalf("GetOrCreate calls = %d, want 1", len(captured))
			}
			if got := captured[0].Backend; got != tc.wantBackend {
				t.Errorf("AgentOpts.Backend = %q, want %q", got, tc.wantBackend)
			}
			// Defensive: cron sessions must always be exempt regardless of
			// the backend choice, so propagation does not regress the
			// no-evict / no-TTL contract for cron-issued spawns.
			if !captured[0].Exempt {
				t.Error("AgentOpts.Exempt = false, want true (cron sessions must stay exempt)")
			}
		})
	}
}
