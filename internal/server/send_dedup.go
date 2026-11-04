// send_dedup.go — the node side of send idempotency (docs/rfc/planner.md
// §15.5 ③): a primary that lost the answer to a send retries it with the same
// turn ID, and the node answers with the first answer instead of running the
// message twice. Held in memory, so it spans reconnects but not a restart.
package server

import (
	"context"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/node"
)

const (
	// sendDedupTTL outlasts any retry a user makes from a "状态未知" bubble.
	sendDedupTTL = 10 * time.Minute
	// sendDedupCap bounds the table against a primary minting turn IDs.
	sendDedupCap = 4096
)

// sendDedup remembers the sends taken per (session key, turn ID).
type sendDedup struct {
	now func() time.Time

	mu    sync.Mutex
	byKey map[string]*dedupEntry
	// order is insertion order; the TTL is uniform, so the oldest is first.
	order []*dedupEntry
}

type dedupEntry struct {
	k    string
	at   time.Time
	done chan struct{}
	// status and taken are written before done closes.
	status string
	taken  bool
}

func newSendDedup() *sendDedup {
	return &sendDedup{now: time.Now, byKey: make(map[string]*dedupEntry)}
}

// do runs send once per (key, turnID). A repeat waits for the first to finish
// and answers its status with Dup set. A first that failed or was not
// buffered (busy) is forgotten, so its repeat runs the send again.
func (d *sendDedup) do(ctx context.Context, key, turnID string, send func() (string, error)) (node.SendResult, error) {
	k := key + "\x00" + turnID
	for {
		d.mu.Lock()
		d.expireLocked()
		e, seen := d.byKey[k]
		if !seen {
			e = &dedupEntry{k: k, at: d.now(), done: make(chan struct{})}
			d.byKey[k] = e
			d.order = append(d.order, e)
		}
		d.mu.Unlock()
		if !seen {
			status, err := d.run(e, send)
			return node.SendResult{Status: status}, err
		}
		select {
		case <-e.done:
		case <-ctx.Done():
			return node.SendResult{}, ctx.Err()
		}
		if e.taken {
			return node.SendResult{Status: e.status, Dup: true}, nil
		}
	}
}

// run is the first send for e; e is released even if send panics.
func (d *sendDedup) run(e *dedupEntry, send func() (string, error)) (status string, err error) {
	defer func() {
		if !e.taken {
			d.mu.Lock()
			if d.byKey[e.k] == e {
				delete(d.byKey, e.k)
			}
			d.mu.Unlock()
		}
		close(e.done)
	}()
	status, err = send()
	e.status, e.taken = status, err == nil && status != string(sendAckBusy)
	return status, err
}

// expireLocked drops expired entries, and the oldest while the table is full
// so the insert that follows keeps it within sendDedupCap.
func (d *sendDedup) expireLocked() {
	cutoff := d.now().Add(-sendDedupTTL)
	for len(d.order) > 0 && (len(d.order) >= sendDedupCap || d.order[0].at.Before(cutoff)) {
		old := d.order[0]
		d.order[0] = nil
		d.order = d.order[1:]
		if d.byKey[old.k] == old {
			delete(d.byKey, old.k)
		}
	}
}
