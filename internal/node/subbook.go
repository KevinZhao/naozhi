package node

import "sync"

// subBook is the subscription ledger shared by wsRelay and ReverseConn: the
// local sinks holding each remote session key, and the newest event time
// seen per key (the `after` a resubscribe sends so the remote does not replay
// full history). Not goroutine-safe; the owning conn's lock guards it.
type subBook struct {
	subs      map[string][]EventSink // remote session key -> local sinks
	lastEvent map[string]int64       // remote session key -> newest event unix ms
}

func newSubBook() subBook {
	return subBook{
		subs:      make(map[string][]EventSink),
		lastEvent: make(map[string]int64),
	}
}

// has reports whether any sink holds key.
func (b *subBook) has(key string) bool { return len(b.subs[key]) > 0 }

// add registers c on key and reports whether c is the key's first sink. A
// sink already on key is not appended twice, or every fan-out would reach it
// twice. Only the first sink seeds lastEvent: a resubscribe racing the first
// forwarded event must not resend after=0, and a later subscriber's older
// `after` must not regress the watermark.
func (b *subBook) add(c EventSink, key string, after int64) (first bool) {
	first = !b.has(key)
	if !containsSink(b.subs[key], c) {
		b.subs[key] = append(b.subs[key], c)
	}
	if first {
		b.lastEvent[key] = after
	}
	return first
}

// remove drops c from key and reports whether the key has no sink left; an
// emptied key loses its watermark too.
func (b *subBook) remove(c EventSink, key string) (empty bool) {
	empty = removeSub(b.subs, key, c)
	if empty {
		delete(b.lastEvent, key)
	}
	return empty
}

// removeAll drops c from every key and returns the keys left without a sink.
func (b *subBook) removeAll(c EventSink) []string {
	emptyKeys := removeSubAll(b.subs, c)
	for _, key := range emptyKeys {
		delete(b.lastEvent, key)
	}
	return emptyKeys
}

// drop forgets key entirely, sinks and watermark.
func (b *subBook) drop(key string) {
	delete(b.subs, key)
	delete(b.lastEvent, key)
}

// reset forgets every key.
func (b *subBook) reset() {
	clear(b.subs)
	clear(b.lastEvent)
}

// observe advances key's watermark to t when t is newer. Keys without a sink
// are ignored so a late event for an unsubscribed key cannot grow the map.
func (b *subBook) observe(key string, t int64) {
	if b.has(key) && t > b.lastEvent[key] {
		b.lastEvent[key] = t
	}
}

// absorb moves every key src holds into b and leaves src empty. A key both
// already hold keeps the older watermark: replaying a few events twice is
// harmless, a gap is not.
func (b *subBook) absorb(src *subBook) {
	for key, sinks := range src.subs {
		if len(sinks) == 0 {
			continue
		}
		after := src.lastEvent[key]
		if b.has(key) {
			after = min(after, b.lastEvent[key])
		}
		for _, s := range sinks {
			if !containsSink(b.subs[key], s) {
				b.subs[key] = append(b.subs[key], s)
			}
		}
		b.lastEvent[key] = after
	}
	src.reset()
}

// resubscription is one key a reconnected conn must subscribe again.
type resubscription struct {
	key   string
	after int64
}

// resubscribeList returns every held key with its watermark.
func (b *subBook) resubscribeList() []resubscription {
	out := make([]resubscription, 0, len(b.subs))
	for key, sinks := range b.subs {
		if len(sinks) > 0 {
			out = append(out, resubscription{key: key, after: b.lastEvent[key]})
		}
	}
	return out
}

// subSnapPool reuses the subscriber snapshot built on every remote event
// (dozens per second during a turn).
var subSnapPool = sync.Pool{
	New: func() any {
		s := make([]EventSink, 0, 16)
		return &s
	},
}

// snapshot copies key's sinks into a pooled slice so the fan-out can run
// outside the owner's lock; the caller must hand the slice back with
// releaseSnapshot.
func (b *subBook) snapshot(key string) *[]EventSink {
	snapPtr := subSnapPool.Get().(*[]EventSink)
	*snapPtr = append((*snapPtr)[:0], b.subs[key]...)
	return snapPtr
}

// releaseSnapshot clears the sink pointers, so disconnected sinks are not
// pinned by the pool, and pools the slice unless a subscriber spike grew it.
func releaseSnapshot(snapPtr *[]EventSink) {
	clients := *snapPtr
	clear(clients)
	if cap(clients) <= 256 {
		*snapPtr = clients[:0]
		subSnapPool.Put(snapPtr)
	}
}
