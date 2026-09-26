package server

import (
	"sync"
	"sync/atomic"
)

// maxSubscribersPerKey caps distinct WS connections subscribed to one session
// key; otherwise a single token can multiply every event fan-out by N. 20 is
// comfortably above the realistic multi-tab / multi-device working set.
const maxSubscribersPerKey = 20

// maxSubscriptionsPerClient caps distinct session keys one WS connection may
// subscribe to, bounding per-client memory and enumeration fan-out. 50 covers
// the dashboard working set with headroom; clients hitting it should
// re-architect rather than have the cap raised.
const maxSubscriptionsPerClient = 50

// subscriberRegistry is the set of connected clients, which of them are
// authenticated, and who is subscribed to which session key. It owns its
// locks; the Hub reaches it only through these methods.
//
// Two locks, never taken in the opposite order: mu guards clients / byKey /
// each client's clientSubs; authMu guards the authenticated set and nests
// inside mu on the write side. Broadcast snapshots take authMu alone, so
// connect and subscribe churn (mu) does not stall them.
//
// Unsubscribe closures take EventLog locks, so the order is mu → EventLog.
// Methods that hand closures back (remove, drain, expire, swap) leave the
// calling to the caller, after mu is released.
type subscriberRegistry struct {
	mu      sync.RWMutex
	clients map[*wsClient]*clientSubs
	// byKey is the subscriber set per session key, including a subscription
	// still being set up (reserve → install). Its size is the per-key count
	// behind maxSubscribersPerKey.
	byKey map[string]map[*wsClient]struct{}
	// spareSets recycles emptied subscriber sets, so a subscribe that finds
	// no session (reserve, then release) or a panel flip does not allocate
	// a fresh set every time.
	spareSets []map[*wsClient]struct{}
	// countFast mirrors len(byKey[key]) for lock-free readers on the event
	// push path. Written only under mu by joinLocked / leaveLocked / drain; a
	// read at most one critical section stale only changes which marshal path
	// a push takes, or skips a best-effort notice nobody could have received.
	countFast sync.Map // key string -> *atomic.Int32

	// The pad keeps authMu off mu's cache line (128 bytes: Apple silicon
	// lines, x86 adjacent-line prefetch): broadcasts read-lock authMu while
	// churn write-locks mu (BenchmarkHubSnapshotAuthenticated/churn +17%).
	_      [128]byte
	authMu sync.RWMutex
	// authSlice is the authenticated clients, contiguous so a broadcast
	// snapshot is one copy; authIdx is each one's position, for O(1)
	// swap-delete and membership.
	authSlice []*wsClient
	authIdx   map[*wsClient]int
}

// clientSubs is one client's subscriptions, guarded by subscriberRegistry.mu.
type clientSubs struct {
	// unsubs maps each subscribed key to its unsubscribe closure; a
	// placeholder until install replaces it.
	unsubs map[string]func()
	// gen counts subscriptions per key, so a parked eventPushLoop can tell
	// that a newer subscription took the key over.
	gen map[string]uint64
	// releaseAt holds the earliest unix-nano time gen[key] may be deleted.
	// Not at unsubscribe time: a stale eventPushLoop may still be parked in
	// resubscribeEvents' 60s wait and would resume if a fresh subscribe reset
	// the generation to a value it remembers. Populated lazily.
	releaseAt map[string]int64
	// lastSweepNs is the unix-nano time of the last sweepExpired scan.
	lastSweepNs int64
}

func newSubscriberRegistry() *subscriberRegistry {
	return &subscriberRegistry{
		clients: make(map[*wsClient]*clientSubs),
		byKey:   make(map[string]map[*wsClient]struct{}),
		authIdx: make(map[*wsClient]int),
	}
}

// add registers c. A client already authenticated at upgrade (no-token mode,
// auth cookie) joins the authenticated set here; token-mode clients join via
// markAuthenticated.
func (r *subscriberRegistry) add(c *wsClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[c]; !ok {
		r.clients[c] = &clientSubs{unsubs: make(map[string]func()), gen: make(map[string]uint64)}
	}
	if c.authenticated.Load() {
		r.authMu.Lock()
		r.addAuthLocked(c)
		r.authMu.Unlock()
	}
}

// markAuthenticated adds c to the authenticated set; the caller stores
// c.authenticated=true first, so a broadcast that finds c also sees the flag.
// A client already removed stays out: a delayed auth racing the teardown must
// not reinsert it.
func (r *subscriberRegistry) markAuthenticated(c *wsClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[c]; ok {
		r.authMu.Lock()
		r.addAuthLocked(c)
		r.authMu.Unlock()
	}
}

// addAuthLocked is idempotent. Caller holds authMu.
func (r *subscriberRegistry) addAuthLocked(c *wsClient) {
	if _, ok := r.authIdx[c]; ok {
		return
	}
	r.authIdx[c] = len(r.authSlice)
	r.authSlice = append(r.authSlice, c)
}

// removeAuthLocked swap-deletes c in O(1). Caller holds authMu.
func (r *subscriberRegistry) removeAuthLocked(c *wsClient) {
	i, ok := r.authIdx[c]
	if !ok {
		return
	}
	delete(r.authIdx, c)
	last := len(r.authSlice) - 1
	if i != last {
		moved := r.authSlice[last]
		r.authSlice[i] = moved
		r.authIdx[moved] = i
	}
	r.authSlice[last] = nil // let the removed client be GC'd
	r.authSlice = r.authSlice[:last]
}

// remove unregisters c. It returns c's unsubscribe closures for the caller to
// run, the keys that c's departure left with no subscriber, and whether c was
// registered at all (a second remove is a no-op).
func (r *subscriberRegistry) remove(c *wsClient) (unsubs []func(), emptied []string, removed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, ok := r.clients[c]
	if !ok {
		return nil, nil, false
	}
	delete(r.clients, c)
	r.authMu.Lock()
	r.removeAuthLocked(c)
	r.authMu.Unlock()
	if n := len(cs.unsubs); n > 0 {
		unsubs = make([]func(), 0, n)
		for key, unsub := range cs.unsubs {
			unsubs = append(unsubs, unsub)
			if r.leaveLocked(c, key) {
				emptied = append(emptied, key)
			}
		}
	}
	return unsubs, emptied, true
}

// drain unregisters every client for Shutdown, returning them and all their
// unsubscribe closures for the caller to run. A delayed markAuthenticated
// afterwards finds no client and adds nothing.
func (r *subscriberRegistry) drain() (clients []*wsClient, unsubs []func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	clients = make([]*wsClient, 0, len(r.clients))
	for c, cs := range r.clients {
		for _, unsub := range cs.unsubs {
			unsubs = append(unsubs, unsub)
		}
		clients = append(clients, c)
	}
	clear(r.clients)
	for key := range r.byKey {
		delete(r.byKey, key)
		r.countFast.Delete(key)
	}
	r.authMu.Lock()
	clear(r.authSlice)
	r.authSlice = r.authSlice[:0]
	clear(r.authIdx)
	r.authMu.Unlock()
	return clients, unsubs
}

// joinLocked adds c to key's subscriber set. Caller holds mu.
func (r *subscriberRegistry) joinLocked(c *wsClient, key string) {
	set := r.byKey[key]
	if set == nil {
		if n := len(r.spareSets); n > 0 {
			set = r.spareSets[n-1]
			r.spareSets = r.spareSets[:n-1]
		} else {
			set = make(map[*wsClient]struct{})
		}
		r.byKey[key] = set
	}
	set[c] = struct{}{}
	r.setCountFastLocked(key, len(set))
}

// leaveLocked removes c from key's subscriber set, reporting whether the key
// is left with none. Caller holds mu.
func (r *subscriberRegistry) leaveLocked(c *wsClient, key string) bool {
	set := r.byKey[key]
	delete(set, c)
	if len(set) == 0 {
		delete(r.byKey, key)
		r.countFast.Delete(key)
		if set != nil && len(r.spareSets) < maxSpareSets {
			r.spareSets = append(r.spareSets, set)
		}
		return true
	}
	r.setCountFastLocked(key, len(set))
	return false
}

func (r *subscriberRegistry) setCountFastLocked(key string, n int) {
	if v, ok := r.countFast.Load(key); ok {
		v.(*atomic.Int32).Store(int32(n))
		return
	}
	var ctr atomic.Int32
	ctr.Store(int32(n))
	r.countFast.Store(key, &ctr)
}

// maxSpareSets bounds spareSets; beyond it emptied sets go to the GC.
const maxSpareSets = 32

// reserveResult is the outcome of reserve.
type reserveResult int

const (
	reserveOK reserveResult = iota
	// reserveGone: the client is no longer registered (Shutdown drained it).
	reserveGone
	reserveClientFull // maxSubscriptionsPerClient
	reserveKeyFull    // maxSubscribersPerKey
)

// reserve claims c's subscription slot for key before the session lookup, so
// two concurrent subscribes at the cap cannot both pass. The slot holds a
// placeholder until install replaces it or release gives it back. A key c
// already subscribes to is re-subscribed: its old closure runs here, under mu,
// and the slot is reused without a second count.
func (r *subscriberRegistry) reserve(c *wsClient, key string) reserveResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, ok := r.clients[c]
	if !ok {
		return reserveGone
	}
	if old, already := cs.unsubs[key]; already {
		old()
	} else {
		if len(cs.unsubs) >= maxSubscriptionsPerClient {
			return reserveClientFull
		}
		if len(r.byKey[key]) >= maxSubscribersPerKey {
			return reserveKeyFull
		}
		r.joinLocked(c, key)
	}
	cs.unsubs[key] = func() {}
	return reserveOK
}

// release gives back c's slot for key without an unsubscribe to run: the
// subscribe did not complete.
func (r *subscriberRegistry) release(c *wsClient, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cs, ok := r.clients[c]; ok {
		if _, ok := cs.unsubs[key]; ok {
			delete(cs.unsubs, key)
			r.leaveLocked(c, key)
		}
	}
}

// install completes a subscription: unsub replaces the placeholder and the
// key's generation advances. admit runs under mu and may decline (the Hub is
// shutting down), in which case the slot is released instead; the caller then
// runs unsub itself. It returns the new generation.
func (r *subscriberRegistry) install(c *wsClient, key string, unsub func(), admit func() bool) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, ok := r.clients[c]
	if !ok || !admit() {
		if ok {
			if _, held := cs.unsubs[key]; held {
				delete(cs.unsubs, key)
				r.leaveLocked(c, key)
			}
		}
		return 0, false
	}
	if _, held := cs.unsubs[key]; !held {
		r.joinLocked(c, key)
	}
	cs.unsubs[key] = unsub
	cs.gen[key]++
	// The key is live again, so a pending reclamation marker is stale: a
	// sweep would delete gen[key] under an active subscription and break
	// resubscribeEvents' takeover detection.
	delete(cs.releaseAt, key)
	return cs.gen[key], true
}

// unsubscribe ends c's subscription to key, running its closure under mu. It
// reports whether key is left with no subscriber.
func (r *subscriberRegistry) unsubscribe(c *wsClient, key string, nowNanos int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, ok := r.clients[c]
	if !ok {
		return false
	}
	unsub, ok := cs.unsubs[key]
	if !ok {
		return false
	}
	unsub()
	return r.dropLocked(c, cs, key, nowNanos)
}

// expire ends c's subscription to key like unsubscribe, but hands the closure
// back for the caller to run after mu is released.
func (r *subscriberRegistry) expire(c *wsClient, key string, nowNanos int64) (stale func(), emptied bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, ok := r.clients[c]
	if !ok {
		return nil, false
	}
	stale, ok = cs.unsubs[key]
	if !ok {
		return nil, false
	}
	return stale, r.dropLocked(c, cs, key, nowNanos)
}

// dropLocked removes key from c's subscriptions and schedules its generation
// for reclamation. gen[key] itself stays: a stale eventPushLoop may still be
// parked in resubscribeEvents, and a fresh subscribe restarting the count at
// 1 would let a remembered gen=1 silently resume.
func (r *subscriberRegistry) dropLocked(c *wsClient, cs *clientSubs, key string, nowNanos int64) bool {
	delete(cs.unsubs, key)
	emptied := r.leaveLocked(c, key)
	if cs.releaseAt == nil {
		cs.releaseAt = make(map[string]int64)
	}
	cs.releaseAt[key] = nowNanos + subGenRetentionNanos
	cs.sweepExpired(nowNanos)
	return emptied
}

// sweepExpired reclaims generations whose retention has passed. It scans at
// most once per subGenSweepMinIntervalNanos unless releaseAt has grown past
// subGenHighWaterMark, which bounds memory on clients flipping many panels.
// Returns the number reclaimed. Caller holds the registry's mu.
func (cs *clientSubs) sweepExpired(nowNanos int64) int {
	if len(cs.releaseAt) == 0 {
		return 0
	}
	if len(cs.releaseAt) < subGenHighWaterMark &&
		nowNanos-cs.lastSweepNs < subGenSweepMinIntervalNanos {
		return 0
	}
	cs.lastSweepNs = nowNanos
	reclaimed := 0
	for key, releaseAt := range cs.releaseAt {
		if nowNanos < releaseAt {
			continue
		}
		// A key subscribed again carries a stale marker; keep its gen.
		if _, active := cs.unsubs[key]; active {
			delete(cs.releaseAt, key)
			continue
		}
		delete(cs.gen, key)
		delete(cs.releaseAt, key)
		reclaimed++
	}
	return reclaimed
}

// generation returns key's current subscription generation for c; ok is false
// once c is unregistered.
func (r *subscriberRegistry) generation(c *wsClient, key string) (gen uint64, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cs, ok := r.clients[c]
	if !ok {
		return 0, false
	}
	return cs.gen[key], true
}

// swap installs unsub for key if gen is still current, handing back the
// closure it replaces for the caller to run after mu is released. ok is false
// (nothing changed) once c is unregistered or a newer subscription took over.
func (r *subscriberRegistry) swap(c *wsClient, key string, gen uint64, unsub func()) (old func(), ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, ok := r.clients[c]
	if !ok || cs.gen[key] != gen {
		return nil, false
	}
	old, held := cs.unsubs[key]
	if !held {
		r.joinLocked(c, key)
	}
	cs.unsubs[key] = unsub
	return old, true
}

// subscribersOf appends key's subscribers to dst. Every subscriber is
// authenticated: readPump refuses a subscribe before auth, and auth is never
// withdrawn.
func (r *subscriberRegistry) subscribersOf(key string, dst []*wsClient) []*wsClient {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for c := range r.byKey[key] {
		dst = append(dst, c)
	}
	return dst
}

// authenticated copies the authenticated clients into dst, reusing its
// capacity.
func (r *subscriberRegistry) authenticated(dst []*wsClient) []*wsClient {
	r.authMu.RLock()
	defer r.authMu.RUnlock()
	n := len(r.authSlice)
	if n == 0 {
		return dst[:0]
	}
	if cap(dst) < n {
		dst = make([]*wsClient, n)
	} else {
		dst = dst[:n]
	}
	copy(dst, r.authSlice)
	return dst
}

// count is key's subscriber count from the lock-free mirror.
func (r *subscriberRegistry) count(key string) int32 {
	v, ok := r.countFast.Load(key)
	if !ok {
		return 0
	}
	return v.(*atomic.Int32).Load()
}

// singleSubscriber reports whether key has exactly one subscriber; 0 is
// false, so only the strict single-tab case takes the fast path.
func (r *subscriberRegistry) singleSubscriber(key string) bool {
	return r.count(key) == 1
}
