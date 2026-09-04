package session

// SessionRole tags the semantic role of a ManagedSession (#728): callers that
// want a structural test ("is this a cron stub?") switch on Role() instead of
// re-deriving the `IsCronKey || ...` prefix expression locally.
type SessionRole int

const (
	// RoleUnknown is the zero value; never returned by Role() — present
	// so default-constructed values surface as obviously-uninitialised
	// rather than silently aliasing RoleIM.
	RoleUnknown SessionRole = iota
	// RoleIM is the standard IM-shape session
	// ({platform}:{chatType}:{id}:{agentID}). The default for any key
	// that does not match a reserved namespace prefix.
	RoleIM
	// RoleCron is a cron-scheduler-owned stub. Key shape "cron:{jobID}";
	// see RegisterCronStub.
	RoleCron
	// RoleProject is a project-scoped planner session. Key shape
	// "project:{name}:planner".
	RoleProject
	// RoleScratch is the dashboard "scratch drawer" follow-up session
	// (non-exempt, ephemeral). Key shape "scratch:...".
	RoleScratch
	// RoleSys is a naozhi-internal background daemon session
	// (AutoTitler etc.). Key shape "sys:{daemon-name}".
	RoleSys
)

// String returns the lower-case role label used in metrics / logs.
func (r SessionRole) String() string {
	switch r {
	case RoleIM:
		return "im"
	case RoleCron:
		return "cron"
	case RoleProject:
		return "project"
	case RoleScratch:
		return "scratch"
	case RoleSys:
		return "sys"
	default:
		return "unknown"
	}
}

// classifyKey returns the role implied by a session key's namespace
// prefix. Pure function on the key alone — separate from
// ManagedSession.Role() so callers that have a key but no live session
// (e.g. router validators) can ask the same question.
//
// The check order matches keyNamespaces in key.go; if a future namespace
// is added, update both sides AND the keyNamespaces table.
func classifyKey(key string) SessionRole {
	switch {
	case IsCronKey(key):
		return RoleCron
	case IsScratchKey(key):
		return RoleScratch
	case IsSysKey(key):
		return RoleSys
	case isPlannerKey(key):
		return RoleProject
	default:
		return RoleIM
	}
}

// Role returns the semantic role of this session, derived from its key
// namespace. Lock-free.
func (s *ManagedSession) Role() SessionRole {
	return classifyKey(s.key)
}
