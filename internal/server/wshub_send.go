// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     send block (queue / sendWG / sendTrackMu / sendClosed /
//	            droppedTotal / legacySendInvokes) +
//	            rate-limit/cache block (userSendLimitersMu / userSendLimiters)
//	READS:      shared deps block (read-only after ctor)
//	READS-ALSO: subscriber block (clients) for per-subscription routing
package server

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// wsFileNotFoundMsg is the WS-path TakeAll miss label: the HTTP sentinel text
// plus a recovery hint, because the WS upload owner may simply be stale (#2418).
const wsFileNotFoundMsg = "file not found or expired，请重新添加附件后再发送"

// remoteNodeProxyTimeout bounds a single proxied interrupt/send RPC to an
// owning peer node before the dashboard goroutine gives up.
const remoteNodeProxyTimeout = 10 * time.Second

// lookupNode resolves a node ID to its Conn via the shared node registry; it
// is the Hub's only by-ID access to the node table. Callers MUST validate the
// ID with isValidNodeID first.
func (h *Hub) lookupNode(id string) (node.Conn, bool) {
	return h.nodes.NodeByID(id)
}

func (h *Hub) handleSend(c *wsClient, msg node.ClientMsg) {
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteSend(c, msg)
		return
	}

	key := msg.Key
	if key == "" {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: "key is required"}))
		return
	}
	// Same trust-boundary gate every other ws path (subscribe / unsubscribe /
	// interrupt) applies: reject C1/bidi/newline bytes and cap length at
	// MaxSessionKeyBytes before the key reaches the dispatch queue, slog attrs,
	// or sessions.json.
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: "invalid key"}))
		return
	}
	if msg.Text == "" && len(msg.FileIDs) == 0 {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: "text or files required"}))
		return
	}
	// Per-field byte cap. wsMaxMessageSize bounds the whole frame, but queued
	// max-size payloads get concatenated by CoalesceMessages into a single
	// stdin write; maxCoalescedTextBytes and maxStdinLineBytes backstop that.
	if len(msg.Text) > maxWSSendTextBytes {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: "text too long"}))
		return
	}
	if len(msg.FileIDs) > maxFilesPerSend {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: errTooManyFiles}))
		return
	}

	// Resolve pre-uploaded file IDs — ownership-checked to prevent cross-user
	// theft. TakeAll is atomic: partial failure leaves the store untouched so
	// the user can retry with a fresh batch. The owner is the one frozen at WS
	// upgrade (never refreshed in no-token mode) and can diverge from the one
	// /api/sessions/upload used; the bundled dashboard sends files over HTTP (#2418).
	var images []cli.Attachment
	if len(msg.FileIDs) > 0 {
		if h.uploadStore == nil {
			c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: "uploads not configured"}))
			return
		}
		taken, err := h.uploadStore.TakeAll(msg.FileIDs, c.uploadOwnerKey())
		if err != nil {
			// Never echo fids (user-controlled) back in the error; log internally.
			slog.Debug("ws send: one or more file_ids not found or expired", "count", len(msg.FileIDs))
			c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: wsFileNotFoundMsg}))
			return
		}
		images = append(images, taken...)
	}

	// Persist file_ref attachments (PDFs) into the workspace, mirroring HTTP
	// handleSend; otherwise the entry reaches NewUserMessageWithMeta with an
	// empty WorkspacePath and its Read-tool bullet is silently dropped.
	var wsRollback func()
	if hasPersistableAttachment(images) {
		// resolveAttachmentWorkspace falls back to the session's saved workspace:
		// the dashboard does not re-send workspace for a running session.
		validatedWS, err := resolveAttachmentWorkspace(h, key, msg.Workspace)
		if err != nil {
			slog.Warn("ws attachment workspace validation failed",
				"key", key, "err", err)
			c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: "invalid workspace"}))
			return
		}
		resolved, rb, perr := persistFileRefs(validatedWS, images, key, c.uploadOwnerKey())
		if perr != nil {
			c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: perr.msg}))
			return
		}
		images = resolved
		wsRollback = rb
	}

	capturedID, capturedKey := msg.ID, key
	reset, status, err := h.sessionSend(sendParams{
		Key:           key,
		Text:          msg.Text,
		Images:        images,
		Workspace:     msg.Workspace,
		ResumeID:      msg.ResumeID,
		Backend:       msg.Backend,
		AccessProfile: msg.AccessProfile,
	}, func(_ error, errMsg string) {
		// Originator-only channel: informational outcomes (/urgent abort,
		// reset) are reported here on purpose — the sender wants to know.
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: capturedID, Status: "error", Key: capturedKey, Error: errMsg}))
	})
	if err != nil {
		if wsRollback != nil {
			wsRollback()
		}
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: asyncErrorMessage(err)}))
		return
	}
	// sessionSend accepted (or reset-processed) the request — files stay on
	// disk and wsRollback must NOT be invoked below this point. On the reset
	// branch (/clear, /new) the files are kept deliberately: the attachments/
	// tree remains an audit record and a re-attached PDF dedups by content hash;
	// the attachments GC sweeper reclaims refs no session picks up (#1394).
	_ = wsRollback
	if reset {
		// HTTP path reports "reset"; keep the WS path in sync so clients can
		// distinguish reset from accepted/queued turns.
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "reset", Key: key}))
		return
	}
	c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: string(status), Key: key}))
}

func (h *Hub) handleInterrupt(c *wsClient, msg node.ClientMsg) {
	key := msg.Key
	if key == "" {
		c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: msg.ID, Status: "error", Error: "key is required"}))
		return
	}
	// Same policy as handleSubscribe / HTTP handlers: reject C1 / bidi /
	// multi-KB keys before they reach router lookup + slog attrs.
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: msg.ID, Status: "error", Error: "invalid key"}))
		return
	}

	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteInterrupt(c, msg)
		return
	}

	// Prefer the non-destructive control_request path so the CLI subprocess
	// survives; raw SIGINT kills Claude `-p`, tearing down the shim and forcing
	// a fresh spawn on the next message. See Router.InterruptSessionSafe.
	switch h.router.InterruptSessionSafe(key) {
	case session.InterruptSent:
		slog.Info("session interrupted via dashboard", "key", key)
		c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: msg.ID, Status: "ok", Key: key}))
	case session.InterruptNoSession:
		c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: msg.ID, Status: "not_running", Key: key}))
	default:
		// control_request returned a non-terminal outcome AND the SIGINT
		// fallback also failed (e.g. session evicted mid-call). Treat as
		// not_running so the dashboard re-queries state.
		c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: msg.ID, Status: "not_running", Key: key}))
	}
}

func (h *Hub) handleRemoteInterrupt(c *wsClient, msg node.ClientMsg) {
	if !isValidNodeID(msg.Node) {
		c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: msg.ID, Status: "error", Key: msg.Key, Error: "unknown node"}))
		return
	}
	nodeID := msg.Node
	conn, ok := h.lookupNode(nodeID)
	if !ok {
		slog.Debug("ws interrupt: unknown node", "node", nodeID)
		c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: msg.ID, Status: "error", Key: msg.Key, Error: "unknown node"}))
		return
	}
	// Only a single proxy RPC is forwarded here, so depend on the narrow
	// node.NodeProxy role rather than the full Conn (#435).
	var nc node.NodeProxy = conn

	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: msg.ID, Status: "error", Key: msg.Key, Node: nodeID, Error: "server shutting down"}))
		return
	}
	go func() {
		defer release()
		capturedID, capturedKey := msg.ID, msg.Key
		// Malformed RPC payloads from a compromised node could panic inside
		// ProxyInterruptSession's decode path; recover so one node cannot take
		// the whole service down, and reply "error" so the dashboard sees it.
		defer func() {
			if r := recover(); r != nil {
				serverMetrics.PanicRecovered()
				// Panic cause at Error, verbose stack at Debug — stack
				// frames leak internal paths to journald/log aggregators.
				slog.Error("remote ws interrupt goroutine panic",
					"node", nodeID, "key", capturedKey,
					"panic", fmt.Sprintf("%v", r))
				slog.Debug("remote ws interrupt goroutine panic: stack",
					"node", nodeID, "key", capturedKey,
					"stack", string(debug.Stack()))
				c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: capturedID,
					Status: "error", Key: capturedKey, Node: nodeID,
					Error: "internal error"}))
			}
		}()
		ctx, cancel := context.WithTimeout(h.ctx, remoteNodeProxyTimeout)
		defer cancel()
		interrupted, err := nc.ProxyInterruptSession(ctx, capturedKey)
		if err != nil {
			// err comes from the remote / transport stack and may carry C1 /
			// bidi / LS+PS bytes; sanitise before logging so a compromised peer
			// cannot inject log lines. 512 B matches upstream/connector_rpc.go (#641).
			slog.Error("remote ws interrupt failed", "node", nodeID, "key", capturedKey, "err", osutil.SanitizeForLog(err.Error(), 512))
			errMsg := "remote interrupt failed"
			if isUnknownRPCMethodErr(err) {
				// Explicit hint so the dashboard toast tells the operator
				// why the action is rejected instead of burying the cause.
				errMsg = "remote node needs upgrade to support this action"
			}
			c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: capturedID, Status: "error", Key: capturedKey, Node: nodeID, Error: errMsg}))
			// interrupt_ack reaches only the originating tab; fan the failure out
			// to every dashboard subscribed to this remote session. The summary
			// is re-sanitised because it is broadcast verbatim (#433).
			h.broadcastSessionSystemEvent(capturedKey, "中断失败："+osutil.SanitizeForLog(err.Error(), 512))
			return
		}
		status := "ok"
		if !interrupted {
			status = "not_running"
		} else {
			slog.Info("remote session interrupted via dashboard", "node", nodeID, "key", capturedKey)
		}
		c.SendJSON(wsproto.NewInterruptAck(wsproto.InterruptAck{ID: capturedID, Status: status, Key: capturedKey, Node: nodeID}))
	}()
}

func (h *Hub) handleRemoteSend(c *wsClient, msg node.ClientMsg) {
	if !isValidNodeID(msg.Node) {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: "unknown node"}))
		return
	}
	// Syntactic workspace validation on the primary: the remote's own
	// EvalSymlinks check uses the remote's defaults, and an unconfigured
	// defaultWorkspace there would pass any absolute path (e.g. `/etc`).
	if err := validateRemoteWorkspace(msg.Workspace); err != nil {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Key: msg.Key, Error: "invalid workspace"}))
		return
	}
	// Same per-field text cap as handleSend; otherwise a remote-targeted send
	// bypasses the local cap and amplifies via coalesce at the remote shim.
	if len(msg.Text) > maxWSSendTextBytes {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Key: msg.Key, Error: "text too long"}))
		return
	}
	nodeID := msg.Node
	// Same charset/length rule as the HTTP path so a hostile client can't push
	// a 4 KB / control-char bag into the send_ack error echo. Empty backend
	// flows through to the router default.
	if !isValidBackendID(msg.Backend) {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Key: msg.Key, Error: "invalid backend id"}))
		return
	}
	// A session bound to a non-default access profile must not be dispatched
	// remotely: the env overlay is host-local and never crosses the wire, so
	// the remote would spawn on the wrong account. Fail loud before the RPC.
	if err := gateRemoteAccessProfile(h.resolver, nodeID, msg.Key); err != nil {
		slog.Debug("ws send: access-profile remote-dispatch rejected", "node", nodeID, "key", msg.Key, "err", err)
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Key: msg.Key, Error: err.Error()}))
		return
	}
	nc, err := selectNodeForBackend(hubNodeLookup{h}, nodeID, msg.Backend)
	if err != nil {
		slog.Debug("ws send: backend route rejected", "node", nodeID, "backend", msg.Backend, "err", err)
		// ErrNodeMissingCap / ErrUnknownBackend / ErrNodeNotConnected are
		// surfaced verbatim: fixed constants (no host paths, no token bytes).
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Key: msg.Key, Error: err.Error()}))
		return
	}
	if nc == nil {
		// Defensive: msg.Node passed isValidNodeID + non-empty above,
		// so selectNodeForBackend should not return (nil, nil) here.
		// Treat as unknown node to keep the existing 400-ish surface.
		slog.Debug("ws send: unknown node", "node", nodeID)
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Error: "unknown node"}))
		return
	}

	// send_ack is deferred until nc.Send returns so the remote session exists
	// before the browser's follow-up subscribe arrives. TrackSend registers the
	// goroutine with sendWG so Shutdown waits for the in-flight RPC+broadcast,
	// and refuses a send that races Shutdown instead of slipping past clientWG.
	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: msg.ID, Status: "error", Key: msg.Key, Node: nodeID, Error: "server shutting down"}))
		return
	}
	go func() {
		defer release()
		capturedID, capturedKey := msg.ID, msg.Key
		// Same rationale as handleRemoteInterrupt: a panic inside nc.Send from
		// a malformed RPC response must not take the whole service down.
		defer func() {
			if r := recover(); r != nil {
				serverMetrics.PanicRecovered()
				// Same split as handleRemoteInterrupt: cause at Error,
				// stack at Debug. Stack frames expose internal layout.
				slog.Error("remote ws send goroutine panic",
					"node", nodeID, "key", capturedKey,
					"panic", fmt.Sprintf("%v", r))
				slog.Debug("remote ws send goroutine panic: stack",
					"node", nodeID, "key", capturedKey,
					"stack", string(debug.Stack()))
				c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: capturedID,
					Status: "error", Key: capturedKey, Node: nodeID,
					Error: "internal error"}))
			}
		}()
		ctx, cancel := context.WithTimeout(h.ctx, remoteNodeProxyTimeout)
		defer cancel()
		if err := nc.Send(ctx, capturedKey, msg.Text, msg.Workspace); err != nil {
			// err originates from the remote transport and may carry control
			// bytes / bidi overrides; sanitise before logging (#641).
			slog.Error("remote ws send failed", "node", nodeID, "key", capturedKey, "err", osutil.SanitizeForLog(err.Error(), 512))
			// Do not surface the raw err: transport-level messages can leak
			// internal host/port/auth details back to authenticated browser
			// clients. Operators still see the detail in the slog above.
			c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: capturedID, Status: "error", Key: capturedKey, Node: nodeID, Error: "remote send failed"}))
			// send_ack reaches only the originating tab; fan the failure out to
			// every dashboard subscribed to this remote session (whose EventLog
			// lives on the node). Summary is re-sanitised: broadcast verbatim (#433).
			h.broadcastSessionSystemEvent(capturedKey, "发送失败："+osutil.SanitizeForLog(err.Error(), 512))
		} else {
			c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: capturedID, Status: "accepted", Key: capturedKey, Node: nodeID}))
			// Refresh the remote subscription so the connector re-creates
			// its streamEvents goroutine if the previous one exited (e.g.
			// process died between the last subscribe and this send).
			nc.RefreshSubscription(capturedKey)
		}
		h.BroadcastSessionsUpdate()
	}()
}
