// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     send block (queue / sendWG / sendTrackMu / sendClosed /
//	            droppedTotal / legacySendInvokes) +
//	            rate-limit/cache block (userSendLimitersMu / userSendLimiters)
//	READS:      shared deps block (read-only after ctor)
//	READS-ALSO: subscriber block (clients) for per-subscription routing
//
// Phase 4b 起 rule 3b 升级到 AST 字段访问对账时，会校验本文件方法体
// 的字段访问匹配本契约；当前 Phase 0b 仅 marker 存在性。
package server

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// File: wshub_send.go
//
// Dashboard-side send / interrupt handling extracted from wshub.go (R243-ARCH-2
// split). Owns:
//   - handleSend: WS "send" → MessageQueue (preferred) or legacy guard path
//   - handleInterrupt: WS "interrupt" → router.InterruptViaControl on local
//     sessions
//   - handleRemoteInterrupt: forwards "interrupt" to the owning node when the
//     subscribed key is hosted on a peer node (multi-node deployments)
//   - handleRemoteSend: forwards "send" similarly with HTTP fallback for
//     pre-WS-relay nodes
//
// All Hub state used by these helpers stays on *Hub (queue, guard, router,
// nodes, ctx, sendWG, projectMgr…). Pure code-relocation.

func (h *Hub) handleSend(c *wsClient, msg node.ClientMsg) {
	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteSend(c, msg)
		return
	}

	key := msg.Key
	if key == "" {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "key is required"})
		return
	}
	// R183-SEC-M1: the remote-node delegation branch above forwards to
	// handleRemoteSend which re-enters the connector's ValidateSessionKey
	// gate; the local fast path here previously skipped that gate,
	// violating the trust-boundary policy every other ws path (subscribe
	// / unsubscribe / interrupt) already follows. An authenticated WS
	// client could land a multi-KB key containing C1/bidi/newline bytes
	// into the dispatch queue, the sessionSend log attrs, and eventually
	// sessions.json at shutdown. ValidateSessionKey also caps length at
	// MaxSessionKeyBytes (~520 B).
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "invalid key"})
		return
	}
	if msg.Text == "" && len(msg.FileIDs) == 0 {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "text or files required"})
		return
	}
	// Per-field byte cap on the WS path. wsMaxMessageSize already bounds the
	// whole JSON frame, but without this inner gate an authenticated client
	// can land repeated max-size payloads into the dispatch queue; when the
	// queue drains, CoalesceMessages concatenates up to MaxDepth entries into
	// a single stdin write. maxCoalescedTextBytes backstops the merged total,
	// and maxStdinLineBytes (12 MB at the shim) is the hard ceiling.
	// R59-SEC-H1.
	if len(msg.Text) > maxWSSendTextBytes {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "text too long"})
		return
	}
	if len(msg.FileIDs) > maxFilesPerSend {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: errTooManyFiles})
		return
	}

	// Resolve pre-uploaded file IDs — ownership-checked to prevent cross-user theft.
	// Atomic TakeAll: partial failure leaves the store untouched so the user
	// can retry with a fresh upload batch rather than silently losing the
	// earlier images. R37-CONCUR4.
	var images []cli.ImageData
	if len(msg.FileIDs) > 0 {
		if h.uploadStore == nil {
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "uploads not configured"})
			return
		}
		taken, err := h.uploadStore.TakeAll(msg.FileIDs, c.uploadOwner)
		if err != nil {
			// Never echo fids (user-controlled) back in the error; log internally.
			slog.Debug("ws send: one or more file_ids not found or expired", "count", len(msg.FileIDs))
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "file not found or expired"})
			return
		}
		images = append(images, taken...)
	}

	// Persist file_ref attachments (PDFs) into the workspace. Mirrors the
	// HTTP handleSend flow; without this the file_ref entry reaches
	// NewUserMessageWithMeta with an empty WorkspacePath and its Read-tool
	// bullet is silently dropped (see prependFileRefHint's skip branch).
	// That presented to the user as a "[System: The user attached 1 file]"
	// prompt with no path — exactly the bug report that triggered this fix.
	var wsRollback func()
	if hasPersistableAttachment(images) {
		// resolveAttachmentWorkspace falls back to the session/router's
		// saved workspace when msg.Workspace is empty. The dashboard does
		// not re-send workspace on every WS message for an already-running
		// session, so this is the common path; without the fallback every
		// post-first send of a PDF returned "invalid workspace".
		validatedWS, err := resolveAttachmentWorkspace(h, key, msg.Workspace)
		if err != nil {
			slog.Warn("ws attachment workspace validation failed",
				"key", key, "err", err)
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "invalid workspace"})
			return
		}
		resolved, rb, perr := persistFileRefs(validatedWS, images, key, c.uploadOwner)
		if perr != nil {
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: perr.msg})
			return
		}
		images = resolved
		wsRollback = rb
	}

	capturedID, capturedKey := msg.ID, key
	reset, status, err := h.sessionSend(sendParams{
		Key:       key,
		Text:      msg.Text,
		Images:    images,
		Workspace: msg.Workspace,
		ResumeID:  msg.ResumeID,
		Backend:   msg.Backend,
	}, func(errMsg string) {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID, Status: "error", Key: capturedKey, Error: errMsg})
	})
	if err != nil {
		if wsRollback != nil {
			wsRollback()
		}
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: asyncErrorMessage(err)})
		return
	}
	// sessionSend accepted (or reset-processed) the request — files must stay on disk.
	// Below this point wsRollback must NOT be invoked: documentation only,
	// no further branches reference it.
	//
	// R040034-GO-8 (#1394): on the reset branch (`/clear` or `/new`) the
	// turn that uploaded these PDFs was discarded by sessionSend, so no
	// session entry references the persisted bytes. We deliberately keep
	// the files instead of rolling back because:
	//   1. Audit — the workspace's attachments/ tree still records the
	//      user upload; ops can correlate it with logs even though the
	//      message itself was reset away.
	//   2. Next-message reuse — a user who hits /clear by mistake and
	//      immediately re-attaches the same PDF benefits from the upload
	//      already living in the workspace; the next /send dedups via
	//      content hash without a re-upload round trip.
	// The attachments GC sweeper handles eventual cleanup for refs that
	// no session ever picks up; that's the audit-bounded case.
	_ = wsRollback
	if reset {
		// /clear or /new — HTTP path reports "reset"; keep the WS path in sync so
		// clients can uniformly distinguish reset from accepted/queued turns
		// instead of seeing an empty Status string.
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "reset", Key: key})
		return
	}
	c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: string(status), Key: key})
}

func (h *Hub) handleInterrupt(c *wsClient, msg node.ClientMsg) {
	key := msg.Key
	if key == "" {
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Error: "key is required"})
		return
	}
	// R175-SEC-P1: same policy as handleSubscribe / HTTP handlers. Reject
	// C1 / bidi / multi-KB keys before they land in router lookup + slog
	// attrs (both local and remote paths).
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Error: "invalid key"})
		return
	}

	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteInterrupt(c, msg)
		return
	}

	// Prefer the non-destructive control_request path so the CLI subprocess
	// survives. Raw SIGINT via InterruptSession kills Claude `-p` outright,
	// which tears down the shim and forces a brand-new spawn on the next
	// message (losing resume context and leaking socket files). See
	// Router.InterruptSessionSafe for the full design rationale.
	switch h.router.InterruptSessionSafe(key) {
	case session.InterruptSent:
		slog.Info("session interrupted via dashboard", "key", key)
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "ok", Key: key})
	case session.InterruptNoSession:
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "not_running", Key: key})
	default:
		// control_request returned a non-terminal outcome AND the SIGINT
		// fallback also failed (e.g. session evicted mid-call). Treat as
		// not_running so the dashboard re-queries state.
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "not_running", Key: key})
	}
}

func (h *Hub) handleRemoteInterrupt(c *wsClient, msg node.ClientMsg) {
	if !isValidNodeID(msg.Node) {
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: "unknown node"})
		return
	}
	nodeID := msg.Node
	h.nodesMu.RLock()
	nc, ok := h.nodes[nodeID]
	h.nodesMu.RUnlock()
	if !ok {
		slog.Debug("ws interrupt: unknown node", "node", nodeID)
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: "unknown node"})
		return
	}

	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Key: msg.Key, Node: nodeID, Error: "server shutting down"})
		return
	}
	go func() {
		defer release()
		capturedID, capturedKey := msg.ID, msg.Key
		// R175-SEC-P1: malformed RPC payloads from a compromised node could
		// panic inside ProxyInterruptSession's json decode path; without a
		// recover the whole naozhi service goes down, affecting every other
		// session. Mirror the ownerLoop/readLoop defensive pattern: log and
		// reply "error" so the dashboard surfaces the failure.
		defer func() {
			if r := recover(); r != nil {
				metrics.PanicRecoveredTotal.Add(1)
				// Panic cause at Error, verbose stack at Debug — stack
				// frames leak internal paths to journald/log aggregators.
				slog.Error("remote ws interrupt goroutine panic",
					"node", nodeID, "key", capturedKey,
					"panic", fmt.Sprintf("%v", r))
				slog.Debug("remote ws interrupt goroutine panic: stack",
					"node", nodeID, "key", capturedKey,
					"stack", string(debug.Stack()))
				c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: capturedID,
					Status: "error", Key: capturedKey, Node: nodeID,
					Error: "internal error"})
			}
		}()
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
		interrupted, err := nc.ProxyInterruptSession(ctx, capturedKey)
		if err != nil {
			// R217-CR-5 (#641): mirror the upstream connector_rpc.go
			// LogSystemEvent path which routes err.Error() through
			// osutil.SanitizeForLog before surfacing it. The error
			// originates from the remote / transport stack so it can
			// carry C1 controls / bidi / LS+PS that byte-level `<0x20`
			// gates miss; without the sanitiser an authenticated client
			// who controls a compromised peer node could inject log
			// lines into the primary's slog sink. 512 B matches the cap
			// used by upstream/connector_rpc.go.
			slog.Error("remote ws interrupt failed", "node", nodeID, "key", capturedKey, "err", osutil.SanitizeForLog(err.Error(), 512))
			errMsg := "remote interrupt failed"
			if isUnknownRPCMethodErr(err) {
				// Explicit hint so the dashboard toast tells the operator
				// why the action is rejected instead of burying the cause.
				errMsg = "remote node needs upgrade to support this action"
			}
			c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: capturedID, Status: "error", Key: capturedKey, Node: nodeID, Error: errMsg})
			// R176-ARCH-NX (#433): same parity gap as handleRemoteSend — the
			// interrupt_ack reaches only the originating tab, so a second
			// operator who pressed stop on the same remote session sees no
			// signal that the interrupt never reached the node. Fan the
			// failure out to every dashboard subscribed to this session over
			// the shared `event` frame. The remote session's EventLog lives
			// on the node so we cannot append locally; the summary is
			// re-sanitised here (same redaction as the slog above) because it
			// is broadcast verbatim to dashboards.
			h.broadcastSessionSystemEvent(capturedKey, "中断失败："+osutil.SanitizeForLog(err.Error(), 512))
			return
		}
		status := "ok"
		if !interrupted {
			status = "not_running"
		} else {
			slog.Info("remote session interrupted via dashboard", "node", nodeID, "key", capturedKey)
		}
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: capturedID, Status: status, Key: capturedKey, Node: nodeID})
	}()
}

func (h *Hub) handleRemoteSend(c *wsClient, msg node.ClientMsg) {
	if !isValidNodeID(msg.Node) {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "unknown node"})
		return
	}
	// Syntactic workspace validation on the primary. Even though the remote
	// node runs its own EvalSymlinks check, that check uses the remote's
	// defaults; a node whose defaultWorkspace is unconfigured would pass
	// any absolute path through. Reject traversal / control-byte / oversize
	// inputs here so no primary-authenticated dashboard user can have a
	// remote node bind e.g. `/etc` as a Claude workspace. R61-SEC-2.
	if err := validateRemoteWorkspace(msg.Workspace); err != nil {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: "invalid workspace"})
		return
	}
	// Enforce the same per-field text cap as handleSend. Without this gate an
	// authenticated dashboard user who targets a remote node can bypass the
	// local cap and push up to wsMaxMessageSize bytes straight to nc.Send,
	// amplifying input into the remote shim's 12 MB stdin line ceiling via
	// coalesce at the remote end. R62-SEC-1.
	if len(msg.Text) > maxWSSendTextBytes {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: "text too long"})
		return
	}
	nodeID := msg.Node
	// PR #119 review fix: gate msg.Backend with the same charset/length
	// rule HTTP path enforces (send.go:262-272) so a hostile WS client
	// can't push a 4 KB / control-char bag into the rejection error
	// string echoed back via send_ack. Empty backend is allowed and
	// flows through the router default below.
	if !isValidBackendID(msg.Backend) {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: "invalid backend id"})
		return
	}
	// Sprint 6b: backend-aware lookup. selectNodeForBackend handles
	// "node not connected" and "node lacks RequiredNodeCaps for the
	// picked backend" with structured errors so the WS client gets a
	// specific reason (logged + surfaced via send_ack.Error). Empty
	// msg.Backend bypasses the cap check entirely (legacy single-
	// backend deployments and existing claude sessions both land
	// here). Errors flow into a "send rejected" send_ack and bypass
	// the goroutine launch / RPC entirely.
	nc, err := selectNodeForBackend(hubNodeLookup{h}, nodeID, msg.Backend)
	if err != nil {
		slog.Debug("ws send: backend route rejected", "node", nodeID, "backend", msg.Backend, "err", err)
		// Surface ErrNodeMissingCap / ErrUnknownBackend / ErrNodeNotConnected
		// verbatim — the dashboard renders the actionable message; raw
		// internals here are limited to the constants in
		// select_node_for_backend.go (no host paths, no token bytes).
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: err.Error()})
		return
	}
	if nc == nil {
		// Defensive: msg.Node passed isValidNodeID + non-empty above,
		// so selectNodeForBackend should not return (nil, nil) here.
		// Treat as unknown node to keep the existing 400-ish surface.
		slog.Debug("ws send: unknown node", "node", nodeID)
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "unknown node"})
		return
	}

	// send_ack is deferred until nc.Send returns, so the remote session
	// is guaranteed to exist when the browser receives the ack and triggers
	// a subscribe. Sending the ack eagerly (before the RPC) caused a race
	// where the subscribe arrived at the remote before session creation.
	//
	// Track via sendWG so Shutdown waits for in-flight RPC+broadcast to
	// finish before tearing down node connections and client maps. Go via
	// TrackSend so a send initiated just as Shutdown fires is refused here
	// rather than squeezing past the clientWG barrier and then hitting a
	// closed sendWG window.
	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: msg.Key, Node: nodeID, Error: "server shutting down"})
		return
	}
	go func() {
		defer release()
		capturedID, capturedKey := msg.ID, msg.Key
		// R175-SEC-P1: same rationale as handleRemoteInterrupt — a panic
		// inside nc.Send (e.g. malformed RPC response from a compromised
		// node) would otherwise take the whole naozhi service down.
		defer func() {
			if r := recover(); r != nil {
				metrics.PanicRecoveredTotal.Add(1)
				// Same split as handleRemoteInterrupt: cause at Error,
				// stack at Debug. Stack frames expose internal layout.
				slog.Error("remote ws send goroutine panic",
					"node", nodeID, "key", capturedKey,
					"panic", fmt.Sprintf("%v", r))
				slog.Debug("remote ws send goroutine panic: stack",
					"node", nodeID, "key", capturedKey,
					"stack", string(debug.Stack()))
				c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID,
					Status: "error", Key: capturedKey, Node: nodeID,
					Error: "internal error"})
			}
		}()
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
		if err := nc.Send(ctx, capturedKey, msg.Text, msg.Workspace); err != nil {
			// R217-CR-5 (#641): symmetric sanitisation with the interrupt
			// path above and the upstream connector_rpc.go LogSystemEvent
			// site. err originates from the remote transport so it may
			// carry control bytes / bidi overrides; route through
			// osutil.SanitizeForLog to keep journald + dashboard tail
			// rendering safe.
			slog.Error("remote ws send failed", "node", nodeID, "key", capturedKey, "err", osutil.SanitizeForLog(err.Error(), 512))
			// Do not surface the raw err: transport-level messages can leak
			// internal host/port/auth details back to authenticated browser
			// clients. Operators still see the detail in the slog above.
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID, Status: "error", Key: capturedKey, Node: nodeID, Error: "remote send failed"})
			// R176-ARCH-NX (#433): parity with the remote→primary direction
			// (upstream/connector_rpc.go injects LogSystemEvent on send
			// failure). The send_ack above reaches only the originating tab;
			// fan the failure out to every dashboard subscribed to this
			// remote session so a second operator watching the conversation
			// sees the message did not land instead of a silent stall. The
			// remote session's EventLog lives on the node, so we cannot append
			// locally — broadcast over the same `event` frame remote events
			// already use. The summary is re-sanitised here (same redaction as
			// the slog above) because it is broadcast verbatim to dashboards.
			h.broadcastSessionSystemEvent(capturedKey, "发送失败："+osutil.SanitizeForLog(err.Error(), 512))
		} else {
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID, Status: "accepted", Key: capturedKey, Node: nodeID})
			// Refresh the remote subscription so the connector re-creates
			// its streamEvents goroutine if the previous one exited (e.g.
			// process died between the last subscribe and this send).
			nc.RefreshSubscription(capturedKey)
		}
		h.BroadcastSessionsUpdate()
	}()
}
