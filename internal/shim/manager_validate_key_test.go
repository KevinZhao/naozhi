package shim

import (
	"strings"
	"testing"
)

// TestValidateKeyForShim_Contract pins the rule set documented on
// validateKeyForShim's godoc as a contract with session.ValidateSessionKey.
// R237-CR-12 (#719): the two validators were kept in lockstep "via comment
// only" -- without an executable assertion any future drift in either side
// would slip past code review.
//
// We cannot import internal/session here because session -> shim already
// (router_backend.go, router_lifecycle.go, router_shim.go) and Go forbids
// the back-edge. Instead this table mirrors the table in
// internal/session/router_test.go::TestValidateSessionKey verbatim -- when
// session's table grows a row, this one must grow too. The mirroring is
// tracked with the same anchor ("R237-CR-12 contract") in both files.
//
// All non-ASCII payloads use \u escapes so the source itself stays plain
// ASCII; Go rejects raw BOM in source files (illegal byte order mark) and
// raw bidi/zero-width payloads break some terminal renderings.
func TestValidateKeyForShim_Contract(t *testing.T) {
	const maxKeyBytes = 515 // matches session.MaxSessionKeyBytes

	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty rejected", "", true},
		{"plain ascii", "feishu:direct:alice:general", false},
		{"utf8 chinese allowed", "feishu:direct:\u5f20\u4e09:general", false},
		{"trailing tab rejected", "a:b:c\t:d", true},
		{"newline rejected", "a:b:c\n:d", true},
		{"C1 NEL rejected (U+0085)", "a:b:c\u0085:d", true},
		{"C1 U+009F rejected", "a:b:c\u009F:d", true},
		{"DEL rejected", "a:b:c\x7f:d", true},
		{"zero-width space rejected", "a:b:c\u200B:d", true},
		{"RLO rejected", "a:b:c\u202E:d", true},
		{"BOM rejected", "a:b:c\uFEFF:d", true},
		{"LSEP rejected", "a:b:c\u2028:d", true},
		{"PSEP rejected", "a:b:c\u2029:d", true},
		{"invalid utf-8 rejected", "a:b:\xc3\x28:d", true},
		{"NUL rejected", "a:b:c\x00:d", true},
		{"oversized rejected", strings.Repeat("a", maxKeyBytes+1), true},
		{"exact-cap accepted", strings.Repeat("a", maxKeyBytes), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateKeyForShim(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
		})
	}
}

// TestValidateKeyForShim_AcceptsKeyspecShape asserts the canonical 4-segment
// session-key shape used across the codebase passes the shim gate. Pulled
// out of the contract table so a future expansion of legitimate shapes
// (e.g. project: planner keys) lives next to the rationale rather than
// drowning the table.
func TestValidateKeyForShim_AcceptsKeyspecShape(t *testing.T) {
	keys := []string{
		"feishu:direct:U_abc123:general",
		"dashboard:scratch:abc:general",
		"project:my-project:planner",
		"cron:hourly:cron-cr-20260526:general",
	}
	for _, k := range keys {
		if err := validateKeyForShim(k); err != nil {
			t.Errorf("legitimate key %q rejected: %v", k, err)
		}
	}
}
