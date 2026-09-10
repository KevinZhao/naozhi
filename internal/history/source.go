// Package history defines a backend-agnostic interface for loading older
// EventEntry pages from storage that outlives the in-memory ring (claude-code:
// ~/.claude/projects/**/{session-id}.jsonl; other CLIs: their own format or
// nothing). The session layer only asks "up to N entries strictly older than T".
//
// The contract itself, and the backend registry, are in contract.go. They used
// to live in internal/cli, leaving this package holding two aliases that pointed
// at the process manager (#2649 G1-d).
package history
