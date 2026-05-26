// SessionRole / SessionMode classification for ManagedSession.
//
// R222-ARCH-10 (#728): the original ManagedSession exposes its
// "what kind of session is this?" question through four implicit
// fields scattered across struct definitions and helper packages:
//
//   - exempt bool                    — TTL/eviction exemption flag
//   - RegisterCronStub stamping      — cron stub initialiser
//   - key prefix "scratch:"          — scratch namespace check
//   - process == nil + Alive() == false → "Paused" (post-TTL)
//
// Callers that want to ask "is this a cron stub?" or "is the
// process currently live?" reach into different fields and helper
// packages. This file centralises the question into two enum types
// + accessor methods so future code can ask once and switch on a
// single value, and so a code-review pass can grep one place for
// every "implicit semantic tag" lookup.
//
// SCOPE: this is the introduce-the-types step. Existing fields
// stay intact and existing call sites are unchanged — the types
// simply expose a derived view. Migrating individual call sites
// to consume Role/Mode instead of poking the underlying fields is
// follow-up work tracked under the same anchor; doing it in this
// commit would touch 20+ files and break the contract test in
// shutdown_order_contract_test.go which greps source text for the
// underlying field names.
package session

// SessionRole classifies the "namespace" of a ManagedSession. The
// classification is derived from the immutable session key (every
// reserved namespace has its own prefix) plus the exempt flag for
// the cron-stub-without-cron-prefix edge case (RegisterCronStub
// sets exempt=true on a still-cron-prefixed key, but a future
// stub source might land on a non-prefixed key — keep the API
// future-proof).
//
// Stable string forms (via String) match the existing log-attr
// vocabulary so a future refactor can swap log emission to call
// Role().String() without changing dashboards / alert queries.
type SessionRole int

const (
	// RoleUser is an IM-user-facing or dashboard-user-facing session
	// (feishu:..., dashboard:..., local:takeover:...). The default
	// when no reserved-namespace prefix matches.
	RoleUser SessionRole = iota
	// RoleCron is a cron job's CLI session. Exempt from TTL and
	// eviction; key always starts with CronKeyPrefix.
	RoleCron
	// RoleSys is a system-daemon session (auto-titler, planner,
	// other internal long-running clients). Exempt; key starts
	// with SysKeyPrefix.
	RoleSys
	// RoleScratch is an ephemeral right-pane scratch session
	// spawned from the dashboard "ask follow-up" affordance.
	// Not exempt; key starts with the scratch prefix.
	RoleScratch
)

// String returns a stable lowercase tag suitable for log attrs and
// metrics labels. Append-only — never reuse a value once it ships
// to dashboards.
func (r SessionRole) String() string {
	switch r {
	case RoleCron:
		return "cron"
	case RoleSys:
		return "sys"
	case RoleScratch:
		return "scratch"
	case RoleUser:
		return "user"
	default:
		return "unknown"
	}
}

// SessionMode describes the runtime state of a ManagedSession. A
// session can be in exactly one mode at a time, and the mode is
// derived from process liveness rather than stored as an
// independent field — that keeps Mode() race-free with the
// existing atomic.Pointer[processBox] swap and avoids a second
// state machine that could drift from the canonical one.
type SessionMode int

const (
	// ModeActive: process is non-nil and Alive() returns true.
	// The session is currently usable for sending messages.
	ModeActive SessionMode = iota
	// ModePaused: process is nil OR not alive, but the session
	// entry is still registered in r.sessions. The next
	// GetOrCreate will respawn (Resume) the underlying CLI.
	// Common on first-message-after-TTL paths.
	ModePaused
	// ModeStub: exempt session that has not yet been bound to a
	// CLI process — used by RegisterCronStub before the cron
	// scheduler dispatches the first turn. Distinguishes "I am
	// expected to have no live process yet" from ModePaused's
	// "I had one and it died/timed-out".
	ModeStub
)

// String returns a stable lowercase tag for mode; same append-only
// rule as SessionRole.String.
func (m SessionMode) String() string {
	switch m {
	case ModeActive:
		return "active"
	case ModePaused:
		return "paused"
	case ModeStub:
		return "stub"
	default:
		return "unknown"
	}
}

// Role returns the canonical role classification for this session.
// Lock-free — reads only the immutable key field.
func (s *ManagedSession) Role() SessionRole {
	switch {
	case IsCronKey(s.key):
		return RoleCron
	case IsSysKey(s.key):
		return RoleSys
	case IsScratchKey(s.key):
		return RoleScratch
	default:
		return RoleUser
	}
}

// Mode returns the canonical runtime-state classification for this
// session. Lock-free — reads only s.exempt (immutable once set
// during construction) and the atomic.Pointer process slot.
//
// ModeStub takes priority over ModePaused: an exempt session
// without a live process is intentionally stub-only (no respawn
// on the next message), while a non-exempt session with a dead
// process is paused (next message resumes).
func (s *ManagedSession) Mode() SessionMode {
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() {
		if s.exempt {
			return ModeStub
		}
		return ModePaused
	}
	return ModeActive
}
