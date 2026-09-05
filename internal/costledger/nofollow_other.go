//go:build !unix

package costledger

// openNoFollow has no portable equivalent here; openDay's Lstat check is the
// only symlink guard on these platforms.
const openNoFollow = 0
