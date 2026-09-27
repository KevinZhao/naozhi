// Package sessionview holds the value types a consumer of session.Router
// exchanges with it: the options a session is created with, the snapshot a
// session is seen through, the outcomes of creating and interrupting one,
// and the checks a caller-supplied workspace path or label must pass before
// it crosses a trust boundary. They are plain data and functions over leaf
// packages (cli, clievent, ring, osutil), so a consumer can use them without
// importing session itself; session re-exports each under its old name.
package sessionview
