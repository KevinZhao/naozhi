package cron

import (
	"context"
	"errors"
	"sync"
)

// panickingRouter is a SessionRouter whose GetOrCreate panics on first
// call. Used to drive R238-GO-9 (#801): TriggerNow's goroutine must
// recover from executeOpt panics so a single job's failure does not
// crash the calling goroutine (and, transitively, leak the triggerWG
// counter that triggerWG.Done in the caller would otherwise stall).
type panickingRouter struct {
	mu    sync.Mutex
	calls int
}

func (p *panickingRouter) RegisterCronStubWithChain(string, string, string, []string) {}
func (p *panickingRouter) Reset(string)                                               {}
func (p *panickingRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	panic(errors.New("simulated GetOrCreate panic"))
}
