package session

import "sync"

// closingKeys counts, per key, the closes naozhi runs on purpose (Remove,
// Archive, StopSession) from the transaction that starts one until its shim
// socket is gone. The shim reconcile skips such a key: a removal unregisters
// the key before it closes the process, so its still-live shim reads as
// "missing from store" and would be adopted under the key, then die of the
// close as shim_eof, leaving a dead session that health judges failed and
// custody resumes; a stop's dying shim would be reattached the same way.
//
// LOCK: mu is a leaf below the table lock. begin runs inside the
// transaction that unregisters or stops the key and the reconcile reads it
// inside the transaction that looks the key up, so a lookup that misses an
// unregistered key also sees it closing.
type closingKeys struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *closingKeys) begin(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[key]++
}

func (c *closingKeys) end(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[key]--; c.n[key] <= 0 {
		delete(c.n, key)
	}
}

func (c *closingKeys) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[key] > 0
}
