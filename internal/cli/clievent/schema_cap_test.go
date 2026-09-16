package clievent

import (
	"strconv"
	"testing"
)

// TestSchemaCapMismatch covers the predicate both ends of the reverse-node link
// gate on. The two "not a mismatch" rows are the ones that matter: they are the
// reason this is a predicate and not a plain `!slices.Contains`.
func TestSchemaCapMismatch(t *testing.T) {
	cases := []struct {
		name string
		caps []string
		want string
	}{
		{"our own tag", []string{SchemaCap}, ""},
		{"our tag among feature caps", []string{"acp", SchemaCap, "askuser"}, ""},
		{
			// A peer predating capability negotiation. The tag was introduced
			// alongside v1, so no tag means v1 — refusing it would break the
			// upgrade path the tag exists to smooth.
			"no evententry tag at all",
			[]string{"acp", "gemini"},
			"",
		},
		{"nil caps", nil, ""},
		{
			// Unknown feature caps stay a WARN elsewhere; they must not read as a
			// schema disagreement.
			"unknown feature cap is not a schema mismatch",
			[]string{SchemaCap, "some-future-backend"},
			"",
		},
		{"newer peer", []string{"evententry.v2"}, "evententry.v2"},
		{"older peer", []string{"evententry.v0"}, "evententry.v0"},
		{
			// A transitional build that reads both is compatible: ours is among
			// what it speaks. Requires scanning the whole slice, not returning on
			// the first family member seen.
			"peer speaking both, ours second",
			[]string{"evententry.v2", SchemaCap},
			"",
		},
		{"peer speaking two foreign versions reports the first", []string{"evententry.v2", "evententry.v3"}, "evententry.v2"},
		{
			// Prefix-adjacent names are different caps, not versions.
			"prefix lookalike is not in the family",
			[]string{"evententry", "evententryv2"},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SchemaCapMismatch(tc.caps); got != tc.want {
				t.Errorf("SchemaCapMismatch(%v) = %q, want %q", tc.caps, got, tc.want)
			}
		})
	}
}

// TestSchemaCapMatchesSchemaVersion pins the tag to the version it names. The
// bump policy in types.go says the paired constants go red together; this is the
// pairing for this one.
func TestSchemaCapMatchesSchemaVersion(t *testing.T) {
	want := schemaCapPrefix + strconv.Itoa(SchemaVersion)
	if SchemaCap != want {
		t.Errorf("SchemaCap = %q but SchemaVersion = %d implies %q — bump both or neither",
			SchemaCap, SchemaVersion, want)
	}
}
