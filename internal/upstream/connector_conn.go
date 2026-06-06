// connector_conn.go owns the WebSocket connection lifecycle for the
// reverse-connect upstream: write-mutex serialisation, ping/pong, request
// fan-out (with bounded reqSem worker pool + panic recovery), subscribe /
// unsubscribe / ping case dispatch, and the wg drain budget that bounds
// reconnect on a stuck downstream call. RPC method handlers (read by
// handleRequest) live in connector_rpc.go; live event streaming lives in
// connector_subscribe.go. All three files are package upstream — split is
// purely organisational.
package upstream

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

func (c *Connector) handleConn(ctx context.Context, conn *websocket.Conn) error {
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		// If SetWriteDeadline fails (conn half-closed / already closed),
		// skip WriteJSON to avoid a deadline-less write that can block
		// until TCP keepalive expires. Return the underlying error so the
		// caller reconnects instead of silently hanging.
		if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
		return conn.WriteJSON(v)
	}

	// Limit concurrent request handling to avoid unbounded goroutine growth.
	reqSem := make(chan struct{}, 16)

	// connCtx is cancelled when this connection drops, ensuring stream
	// goroutines exit promptly without blocking reconnect.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// activeSubs tracks local session subscriptions initiated by primary.
	// subExited receives keys when streamEvents goroutines exit (channel closed),
	// so the main loop can remove stale entries and allow re-subscription.
	// A generation counter prevents late subExited notifications from deleting
	// a freshly re-created subscription for the same key.
	type subExitNote struct {
		key string
		gen uint64
	}
	activeSubs := map[string]func(){} // key → cancel func
	subGen := map[string]uint64{}     // key → generation counter
	// Capacity 256: streamEvents goroutines drop their subExited note
	// non-blockingly, and the main loop drains between ReadJSON calls. A
	// 64-slot channel could overflow during hub-wide resets (e.g. Router
	// Cleanup sweeping >64 sessions at once while ReadJSON is blocked),
	// leaving stale activeSubs entries. 256 covers realistic burst sizes
	// for all deployments; the reqSem inflight cap is 16 so a 256-deep
	// backlog is a generous safety margin. R71-GO-M1.
	subExited := make(chan subExitNote, 256)

	var wg sync.WaitGroup
	// R51-REL-005: bound the shutdown-of-handleConn on a hard deadline so a
	// stuck worker goroutine (typically a send-RPC blocked on sess.Send that
	// can wait up to CLI watchdog timeout ≈ 5 min) cannot pin reconnect.
	// connCancel() above already fired by the time we reach this defer —
	// every wg participant either responds to connCtx or is inherently
	// short-running (ping ticker, response writer), so the grace timer
	// covers only the pathological case where a downstream Send refuses
	// to honour ctx. Exceeding the budget leaks the stuck goroutine to
	// process teardown, which is strictly better than blocking the whole
	// upstream reconnect loop.
	defer func() {
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		// R180-GO-P1 / R180-PERF-P2: use NewTimer + explicit Stop instead of
		// time.After. time.After always arms a runtime timer; if wg.Wait()
		// finishes fast (the common happy path) the timer goroutine leaks
		// until handleConnDrainBudget (15s) expires. This pattern is already
		// fixed in router.go:713 and shim/manager.go:264.
		drainTimer := time.NewTimer(handleConnDrainBudget)
		defer drainTimer.Stop()
		select {
		case <-done:
		case <-drainTimer.C:
			slog.Warn("connector: handleConn drain exceeded budget, proceeding",
				"budget", handleConnDrainBudget)
		}
	}()

	// Periodically send WebSocket-level pings so pongHandler resets the read deadline.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Hold writeMu across the Close on failure so conn.Close does
				// not race with a concurrent writeJSON that has just entered
				// the critical section. gorilla/websocket requires at most
				// one writer at a time; closing under the lock serializes us
				// against WriteJSON. Any writeJSON that then acquires the
				// lock will see SetWriteDeadline fail (closed conn) and
				// return its error cleanly. Force-close is what breaks the
				// outer ReadJSON out of its 90s pong wait when the peer is
				// dead — we want that to happen even if no Write failed
				// yet, so emit the Close without unlocking first.
				//
				// pingOnce encapsulates the "lock → try write → close on
				// failure" triad in a single scope so a single `defer
				// writeMu.Unlock()` covers every exit. The boolean return
				// lets the outer loop exit without keeping the lock live
				// across the return.
				if !pingOnce(conn, &writeMu) {
					return
				}
			case <-connCtx.Done():
				return
			}
		}
	}()

	// Clean up all event log subscriptions when connection drops.
	defer func() {
		for key, cancel := range activeSubs {
			cancel()
			delete(activeSubs, key)
		}
	}()

	for {
		// Drain stale subscription entries from exited streamEvents goroutines
		// so re-subscribe messages for the same key are accepted.
	drainLoop:
		for {
			select {
			case note := <-subExited:
				if subGen[note.key] == note.gen {
					delete(activeSubs, note.key)
				}
			default:
				break drainLoop
			}
		}

		var msg node.ReverseMsg
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}

		switch msg.Type {
		case "request":
			req := msg
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						// R180-SEC-H1 / R181-GO-P2-1: req.ReqID and req.Method
						// come from the primary's JSON frame with no prior
						// sanitization. A compromised / middleman-tampered
						// primary can inject bidi/C1/newline bytes to forge
						// log entries. SanitizeForLog keeps attrs as plain
						// strings (strips unsafe runes → '_') instead of the
						// Go-quoted form of %q which slog's JSON handler then
						// double-escapes.
						slog.Error("connector request panic",
							"req_id", osutil.SanitizeForLog(req.ReqID, 128),
							"method", osutil.SanitizeForLog(req.Method, 64),
							"panic", r, "stack", string(debug.Stack()))
					}
				}()
				// Two-stage acquire to distinguish "got a slot immediately"
				// from "had to block". The first non-blocking try keeps the
				// happy path identical to the original select{acquire,
				// ctx.Done} (no extra syscall, no extra allocation); only
				// the contended path pays the WaitTotal increment + the
				// outer blocking select. ctx.Done lives on the blocking
				// branch so cancellation semantics are unchanged.
				select {
				case reqSem <- struct{}{}:
				default:
					reqSemReqWaitTotal.Add(1)
					select {
					case reqSem <- struct{}{}:
					case <-ctx.Done():
						return
					}
				}
				// R20260527122801-GO-007: register the release defer BEFORE
				// reqSemReqInflight.Add(1) so a panic between the slot
				// acquire and the inflight increment cannot leave the
				// counter and the semaphore desynced. Both states then
				// roll back together via the deferred release on any
				// downstream panic in handleRequest.
				defer func() {
					<-reqSem
					reqSemReqInflight.Add(-1)
				}()
				reqSemReqInflight.Add(1)
				result, err := c.handleRequest(ctx, connCtx, req, &wg)
				resp := node.ReverseMsg{Type: "response", ReqID: req.ReqID}
				if err != nil {
					resp.Error = err.Error()
				} else {
					resp.Result = result
				}
				if wErr := writeJSON(resp); wErr != nil {
					slog.Debug("connector response write failed", "err", wErr)
				}
			}()

		case "subscribe":
			key := msg.Key
			// R180-SEC-M3: gate the subscribe path at the trust boundary.
			// handleRequest's per-method branches all run ValidateSessionKey,
			// but the subscribe/unsubscribe main-loop cases previously
			// accepted any string and piped it straight into slog attrs +
			// router.GetSession map lookup. A compromised primary could
			// inject bidi/C1/newline bytes via msg.Key.
			if err := session.ValidateSessionKey(key); err != nil {
				slog.Debug("connector subscribe: invalid key", "err", err)
				break
			}
			// #1822 (ARCH-3): if this connection is already draining (connCtx
			// cancelled by connCancel / the parent shutdown cancel), do not spawn
			// a new streamEvents goroutine. The spawnSession stopped gate covers
			// the three reverse-RPC spawn paths but NOT this read-only subscribe,
			// whose only leak is the streamEvents goroutine bounded by the 15s
			// handleConn drain. Short-circuiting here keeps a late subscribe from
			// adding a fresh goroutine that races shutdown. streamEvents itself
			// also observes connCtx, but the wg.Add happens before it runs, so we
			// must reject up front to avoid the wg participant entirely.
			if connCtx.Err() != nil {
				slog.Debug("connector subscribe: connection draining, rejecting", "key", key)
				break
			}
			// Cancel stale subscription if the previous streamEvents goroutine
			// exited (e.g. process died). This allows the hub to re-subscribe
			// after a remote send so events flow for the new process.
			if cancel, already := activeSubs[key]; already {
				cancel()
				delete(activeSubs, key)
			}
			sess := c.router.GetSession(key)
			if sess == nil {
				if err := writeJSON(node.ReverseMsg{Type: "subscribe_error", Key: key, Error: "session not found"}); err != nil {
					slog.Debug("connector write subscribe_error", "key", key, "err", err)
				}
				break
			}
			notify, cancel := sess.SubscribeEvents()
			activeSubs[key] = cancel
			subGen[key]++
			myGen := subGen[key]
			if err := writeJSON(node.ReverseMsg{Type: "subscribed", Key: key}); err != nil {
				slog.Debug("connector write subscribed", "key", key, "err", err)
			}
			wg.Add(1)
			go func(k string, n <-chan struct{}, g uint64) {
				defer wg.Done()
				c.streamEvents(connCtx, writeJSON, k, n)
				// Signal that this subscription exited (session replaced/reset).
				// A dropped notification leaves activeSubs[k] populated until
				// the next explicit subscribe/unsubscribe for the same key
				// clears it — not a correctness bug (cancel is idempotent),
				// but observability for capacity tuning. R71-GO-M1.
				select {
				case subExited <- subExitNote{k, g}:
				default:
					slog.Warn("connector: subExited channel full, activeSubs cleanup delayed", "key", k)
				}
			}(key, notify, myGen)

		case "unsubscribe":
			key := msg.Key
			// R180-SEC-M3: same trust-boundary guard as subscribe.
			if err := session.ValidateSessionKey(key); err != nil {
				slog.Debug("connector unsubscribe: invalid key", "err", err)
				break
			}
			if cancel, ok := activeSubs[key]; ok {
				cancel()
				delete(activeSubs, key)
			}
			if err := writeJSON(node.ReverseMsg{Type: "unsubscribed", Key: key}); err != nil {
				slog.Debug("connector write unsubscribed", "key", key, "err", err)
			}

		case "ping":
			if err := writeJSON(node.ReverseMsg{Type: "pong"}); err != nil {
				slog.Debug("connector write pong", "err", err)
			}
		}
	}
}
