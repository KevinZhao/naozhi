package api

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestEventLogSatisfiesAppenderAndSubscriber_R20260602_091302_ARCH_2 anchors
// #1570: the canonical in-memory ring backend (*cli.EventLog) must already
// satisfy the write + subscribe halves of the unified contract, so adopting
// the api package is a no-cost convergence rather than a rewrite.
func TestEventLogSatisfiesAppenderAndSubscriber_R20260602_091302_ARCH_2(t *testing.T) {
	t.Parallel()
	var l *cli.EventLog
	var _ Appender = l
	var _ Subscriber = l
}

// stubReader is the minimal durable-tier shape: it implements the read side
// (cli.HistorySource) the way naozhilog.Source / merged do.
type stubReader struct{}

func (stubReader) LoadBefore(context.Context, int64, int) ([]cli.EventEntry, error) {
	return nil, nil
}

// fullStore composes the ring's write+subscribe behaviour with a durable
// reader to demonstrate that EventStore is satisfiable by a registry-injected
// composite — the end state #1570 targets.
type fullStore struct {
	*cli.EventLog
	stubReader
}

// TestEventStoreComposable_R20260602_091302_ARCH_2 anchors that EventStore is
// a real, satisfiable contract (Appender + Reader + Subscriber) — a backend
// can be handed to the session layer behind this single interface.
func TestEventStoreComposable_R20260602_091302_ARCH_2(t *testing.T) {
	t.Parallel()
	var _ EventStore = fullStore{}
	var _ Reader = stubReader{}
}
