package envpolicy

import "regexp"

// reProfileValue matches safe AWS profile names; rejects shell metacharacters
// or path separators that could redirect credential_process lookups.
var reProfileValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// IsSafeProfileValue reports whether v is a safe AWS profile name (#1617).
func IsSafeProfileValue(v string) bool {
	return reProfileValue.MatchString(v)
}
