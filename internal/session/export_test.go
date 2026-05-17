package session

// IsPlannerKey exposes the unexported isPlannerKey for cross-package
// contract tests in session_test (R215-CR-P1-1). The duplicate of
// project.IsPlannerKey in this package was introduced to break the
// import cycle session→project; the hardcoded literal mirroring is
// asserted from project_test.go and routing_test.go independently,
// but no test compares the two implementations behavior-for-behavior.
// This hook lets session_test do that without re-exporting the helper
// to production code.
func IsPlannerKey(key string) bool { return isPlannerKey(key) }
