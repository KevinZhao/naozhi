// Package sessionview holds the value types a consumer of session.Router
// exchanges with it: the options a session is created with, the snapshot a
// session is seen through, and the outcomes of creating and interrupting
// one. They are plain data over leaf packages (cli, clievent, ring), so a
// consumer can use them without importing session itself; session re-exports
// each under its old name, so the two spellings are the same type.
package sessionview
