package envpolicy

import "regexp"

// reProfileValue matches safe AWS profile names: alphanumeric plus underscore
// and hyphen, 1-64 characters. Rejects shell metacharacters or path separators
// that could redirect credential_process lookups.
var reProfileValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// IsSafeProfileValue reports whether v is a safe AWS profile name.
// Enforces ^[A-Za-z0-9_-]{1,64}$ to block injection via credential_process.
// R20260603000023-SEC-1 (#1617).
func IsSafeProfileValue(v string) bool {
	return reProfileValue.MatchString(v)
}
