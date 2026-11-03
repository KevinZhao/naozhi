package session

import (
	"cmp"
	"strings"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Cross-session messages (CC's ListAgents / SendMessage channel,
// docs/rfc/planner.md §6.2): the inbound policy a session spawns with and the
// name → key lookup that labels an inbound message's sender.

// crossSessionInboundFor is the crossSessionInbound a Claude session spawns
// with when its caller set none: "hold" for a chat you talk to directly (an
// IM or dashboard session, keyed {platform}:{chatType}:{chatID}:{agentId}),
// so another session's message is only noted there, never acted on; "" (the
// CLI default) for every other namespace (cron, sys, scratch, project
// planners); "accept" for the global Planner (planner:main), which also sets
// it explicitly, and for the sessions it dispatches (task:), so one resumed
// from the dashboard still takes the Planner's messages.
//
// Only fresh spawns read it: a surviving shim's drift check rebuilds its argv
// from the overlay it recorded, so a session spawned before the policy keeps
// running without it until its next spawn.
func crossSessionInboundFor(key string) string {
	if sessionkey.IsPlannerNSKey(key) || sessionkey.IsTaskKey(key) {
		return "accept"
	}
	// The IM key shape, tested as ResolveForKey does: a scratch key has four
	// segments too.
	if !IsReservedNamespace(key) && len(strings.SplitN(key, ":", 4)) == 4 {
		return "hold"
	}
	return ""
}

// SessionKeyForCLIName returns the key of the Claude session whose current
// or last process spawned with --name name, "" when none did (a terminal
// session, or one naozhi does not manage). Kiro and Codex sessions are
// skipped: their CLIs take no --name, so a peer cannot address them by it.
// On a name two sessions share, the smallest key wins, so repeated lookups
// agree.
func (r *Router) SessionKeyForCLIName(name string) string {
	if name == "" {
		return ""
	}
	var found string
	r.ss.View(func(v sessView) {
		for key, s := range v.All() {
			if s.CLISessionName() != name || cmp.Or(s.Backend(), r.backends.bk.DefaultID()) != "claude" {
				continue
			}
			if found == "" || key < found {
				found = key
			}
		}
	})
	return found
}

// peerKeyResolverSetter is the optional hook a process offers to resolve the
// sender of a cross-session message to a session key; see bindPeerKeys.
type peerKeyResolverSetter interface {
	SetPeerKeyResolver(fn func(name string) string)
}

// bindPeerKeys has proc stamp each cross-session message's sender with the
// key SessionKeyForCLIName resolves it to, at arrival: a name a later session
// reuses must not relabel an old message.
func (r *Router) bindPeerKeys(proc processIface) {
	if p, ok := proc.(peerKeyResolverSetter); ok {
		p.SetPeerKeyResolver(r.SessionKeyForCLIName)
	}
}
