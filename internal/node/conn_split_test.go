package node

import "testing"

// TestConnRoleInterfaceSplit pins H6 / #435: the 26-method Conn interface is
// decomposed into four role interfaces (NodeInfo / NodeFetcher / NodeProxy /
// NodeSubscriber). These compile-time assertions guarantee:
//
//  1. Every concrete implementation (*HTTPClient, *ReverseConn) still satisfies
//     each narrow role — so a consumer can depend on just the slice it uses.
//  2. The full Conn remains assignable from those implementations — so no
//     existing call site that takes a Conn breaks.
//  3. A Conn value can be narrowed to any single role interface — the seam
//     wshub consumers rely on to take e.g. NodeProxy instead of the whole Conn.
//
// If a future change removes a method from a role interface (or moves it to a
// different role) without updating call sites, this file stops compiling,
// flagging the regression before it ships.
var (
	_ NodeInfo       = (*HTTPClient)(nil)
	_ NodeFetcher    = (*HTTPClient)(nil)
	_ NodeProxy      = (*HTTPClient)(nil)
	_ NodeSubscriber = (*HTTPClient)(nil)
	_ Conn           = (*HTTPClient)(nil)

	_ NodeInfo       = (*ReverseConn)(nil)
	_ NodeFetcher    = (*ReverseConn)(nil)
	_ NodeProxy      = (*ReverseConn)(nil)
	_ NodeSubscriber = (*ReverseConn)(nil)
	_ Conn           = (*ReverseConn)(nil)
)

// TestConnNarrowsToRoles asserts at runtime that a Conn can be passed wherever
// a single role interface is expected — the whole point of the split. A plain
// compile-time check would not exercise the assignment through a Conn-typed
// variable, which is the shape wshub consumers actually use.
func TestConnNarrowsToRoles(t *testing.T) {
	var c Conn = (*HTTPClient)(nil)

	var info NodeInfo = c
	var fetch NodeFetcher = c
	var proxy NodeProxy = c
	var sub NodeSubscriber = c

	if info == nil || fetch == nil || proxy == nil || sub == nil {
		t.Fatal("Conn must narrow to every role interface (H6 / #435)")
	}
}
