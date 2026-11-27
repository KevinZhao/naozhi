package discovery

// maxSafeJSONInt mirrors JavaScript's Number.MAX_SAFE_INTEGER (2^53 - 1). A
// uint64 that crosses a JSON boundary into dashboard.js or a reverse-RPC peer
// must stay below it or JSON.parse rounds it and PID-identity comparisons
// (ProcStartTime) match wrongly; proc_*_test.go hold ProcStartTime to it.
const maxSafeJSONInt uint64 = (1 << 53) - 1
