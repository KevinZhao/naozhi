//go:build unix

package costledger

import "syscall"

// openNoFollow makes OpenFile refuse a symlink at the final path component,
// backing the Lstat check in openDay against a swap between the two calls.
const openNoFollow = syscall.O_NOFOLLOW
