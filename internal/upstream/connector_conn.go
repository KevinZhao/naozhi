// connector_conn.go owns the WebSocket connection lifecycle for the
// reverse-connect upstream: write serialisation, ping/pong, bounded request
// fan-out, subscribe/unsubscribe dispatch, and the wg drain budget. RPC
// handlers live in connector_rpc.go; event streaming in connector_subscribe.go.
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
		// A failed SetWriteDeadline means the conn is half-closed; skip
		// WriteJSON so we don't block until TCP keepalive expires.
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

	// activeSubs tracks subscriptions initiated by the primary. streamEvents
	// goroutines report exit via subExited so stale entries can be removed;
	// the generation counter keeps a late note from deleting a re-created
	// subscription for the same key.
	type subExitNote struct {
		key string
		gen uint64
	}
	activeSubs := map[string]func(){} // key → cancel func
	subGen := map[string]uint64{}     // key → generation counter
	// 256 slots absorb hub-wide resets (Router Cleanup sweeping many sessions
	// while ReadJSON is blocked) without dropping exit notes.
	subExited := make(chan subExitNote, 256)

	var wg sync.WaitGroup
	// Bound the drain on handleConnDrainBudget so a worker stuck in sess.Send
	// (up to the CLI watchdog ≈5 min) cannot pin reconnect. connCancel must
	// run FIRST here: the top-level `defer connCancel()` runs after this
	// defer, so without it the ping goroutine would stay parked for the whole
	// budget on a plain ReadJSON-error disconnect (#2222).
	defer func() {
		connCancel()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		// NewTimer + Stop rather than time.After so the timer does not linger
		// for the full budget on the fast path.
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
				// pingOnce holds writeMu across the Close so conn.Close does not
				// race a concurrent writeJSON; the force-close is what breaks the
				// outer ReadJSON out of its pong wait when the peer is dead.
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
						// req.ReqID / req.Method come unsanitized from the primary's
						// frame; SanitizeForLog strips bidi/C1/newline bytes so a
						// tampered primary cannot forge log entries.
						slog.Error("connector request panic",
							"req_id", osutil.SanitizeForLog(req.ReqID, 128),
							"method", osutil.SanitizeForLog(req.Method, 64),
							"panic", r, "stack", string(debug.Stack()))
					}
				}()
				// Non-blocking try first so the uncontended path pays nothing;
				// only the contended path counts WaitTotal.
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
				// Register the release defer BEFORE the inflight increment so a
				// panic between them cannot desync counter and semaphore.
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
			// Trust-boundary gate: msg.Key flows into slog attrs and the
			// router.SessionFor lookup; reject bidi/C1/newline bytes up front.
			if err := session.ValidateSessionKey(key); err != nil {
				slog.Debug("connector subscribe: invalid key", "err", err)
				break
			}
			// Reject subscribes on a draining connection: the wg.Add below runs
			// before streamEvents observes connCtx, so a late subscribe would add
			// a fresh wg participant racing shutdown (#1822).
			if connCtx.Err() != nil {
				slog.Debug("connector subscribe: connection draining, rejecting", "key", key)
				break
			}
			// Cancel a stale subscription so the hub can re-subscribe after a
			// remote send and events flow for the new process.
			if cancel, already := activeSubs[key]; already {
				cancel()
				delete(activeSubs, key)
			}
			sess := c.router.SessionFor(key)
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
				// Signal exit so the main loop drops activeSubs[k]. A dropped note
				// only delays cleanup until the next subscribe/unsubscribe for k.
				select {
				case subExited <- subExitNote{k, g}:
				default:
					slog.Warn("connector: subExited channel full, activeSubs cleanup delayed", "key", k)
				}
			}(key, notify, myGen)

		case "unsubscribe":
			key := msg.Key
			// Same trust-boundary guard as subscribe.
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
