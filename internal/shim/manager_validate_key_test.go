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

// TestValidateKeyForShim_RuneClassBoundaries pins the EXACT rune-class
// boundaries the validator enforces. The table-based contract test above
// only covers sample points — boundary tests catch off-by-one drift in
// either direction (e.g. someone "fixes" 0x9F → 0xA0 thinking C1 ends at
// the next codepoint, or someone widens the bidi-marks range from
// 0x202E to 0x202F).
//
// R237-CR-12 (#719): the cross-package "keep in sync" guarantee with
// session.ValidateSessionKey now has TWO layers — the existing
// TestValidateKeyForShim_Contract sample table plus this boundary
// table. A unilateral edit to validateKeyForShim that only adjusts a
// single bound (e.g. drops the C1 reject) will fail this test
// regardless of whether the session-side table has been updated, so
// at minimum the local validator's stated contract is mechanically
// pinned. Cross-validator parity remains a manual review step until
// the shared rule set is extracted into the leaf keyspec package
// (the issue's longer-term proposal, blocked on a domain split).
func TestValidateKeyForShim_RuneClassBoundaries(t *testing.T) {
	type rc struct {
		name    string
		r       rune
		wantErr bool
	}
	cases := []rc{
		// C0 control range U+0000..U+001F: every codepoint must reject.
		{"NUL (U+0000)", 0x0000, true},
		{"BS (U+0008)", 0x0008, true},
		{"TAB (U+0009)", 0x0009, true},
		{"LF (U+000A)", 0x000A, true},
		{"US (U+001F) — last C0", 0x001F, true},

		// First printable ASCII U+0020 (space) must accept — boundary +1.
		{"SPACE (U+0020) — first printable", 0x0020, false},

		// Last printable ASCII U+007E (~) must accept — DEL boundary -1.
		{"TILDE (U+007E)", 0x007E, false},
		{"DEL (U+007F)", 0x007F, true},

		// C1 range U+0080..U+009F: every codepoint must reject.
		{"PAD (U+0080) — first C1", 0x0080, true},
		{"NEL (U+0085)", 0x0085, true},
		{"APC (U+009F) — last C1", 0x009F, true},

		// First post-C1 codepoint U+00A0 (NBSP) must accept.
		{"NBSP (U+00A0) — post-C1", 0x00A0, false},

		// Bidi/zero-width range U+200B..U+200F.
		{"ZWSP (U+200A) — pre-bidi", 0x200A, false}, // outside the range, accept
		{"ZWSP (U+200B) — first bidi", 0x200B, true},
		{"LRM (U+200E)", 0x200E, true},
		{"RLM (U+200F) — last bidi-mark", 0x200F, true},
		{"WJ (U+2060) — post-bidi-mark", 0x2060, false},

		// LSEP/PSEP single codepoints.
		{"LSEP (U+2028)", 0x2028, true},
		{"PSEP (U+2029)", 0x2029, true},

		// Bidi-embedding range U+202A..U+202E.
		{"LRE (U+202A) — first embedding", 0x202A, true},
		{"RLO (U+202E) — last embedding", 0x202E, true},
		{"NNBSP (U+202F) — post-embedding", 0x202F, false},

		// BOM single codepoint.
		{"BOM (U+FEFF)", 0xFEFF, true},
		{"pre-BOM (U+FEFE)", 0xFEFE, false},
		{"post-BOM (U+FF00)", 0xFF00, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Embed the rune in an otherwise-valid 4-segment key so the
			// length / shape checks don't shadow the rune-class check.
			k := "a:b:c" + string(c.r) + ":d"
			err := validateKeyForShim(k)
			if (err != nil) != c.wantErr {
				t.Fatalf("rune %#x: err=%v, wantErr=%v", c.r, err, c.wantErr)
			}
		})
	}
}
