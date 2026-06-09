package textutil

import (
	"strings"
	"testing"
)

// TestMayContainSecretPrefixCoversAllFirstBytes is a CI invariant asserting
// that mayContainSecretPrefix's internal byteset scan covers the first byte
// of every entry in secretPrefixes.
//
// R20260602-091302-CR-1: the fast-path scan set "sgAxhny" has no structural
// link to the secretPrefixes table. If a new prefix is added whose first byte
// is absent from the scan string, mayContainSecretPrefix would return false
// for inputs that start with that byte, silently bypassing all redaction.
// This test ensures that omission is caught by CI before reaching production.
func TestMayContainSecretPrefixCoversAllFirstBytes(t *testing.T) {
	for _, sp := range secretPrefixes {
		if len(sp.prefix) == 0 {
			t.Errorf("secretPrefixes contains an entry with an empty prefix")
			continue
		}
		firstByte := string(sp.prefix[0])
		if !mayContainSecretPrefix(firstByte) {
			t.Errorf("mayContainSecretPrefix scan set does not cover first byte %q of prefix %q — add %q to the scan string in mayContainSecretPrefix",
				firstByte, sp.prefix, firstByte)
		}
		// Cross-check via strings.IndexAny as the canonical source of truth:
		// if the byte is absent from the scan set literal, both this call and
		// the production call will miss it, so the test above is the real guard.
		_ = strings.IndexAny(firstByte, "sgAxhny") // compile-time sanity only
	}
}

// TestSecretPrefixIndexCoversAllPrefixes is a CI invariant for the
// R20260609-PERF-9 (#1976) first-byte bucket optimization: RedactSecrets now
// probes only secretPrefixesByFirstByte[s[i]] instead of the full table, so
// every prefix MUST be reachable through its first-byte bucket. If a future
// prefix were added to secretPrefixes but not the index (e.g. index built from
// a stale copy), that secret would silently bypass redaction. This asserts the
// index is a faithful, complete, order-preserving partition of secretPrefixes.
func TestSecretPrefixIndexCoversAllPrefixes(t *testing.T) {
	// Every prefix must appear in its first-byte bucket, in declaration order.
	indexed := 0
	for _, bucket := range secretPrefixesByFirstByte {
		indexed += len(bucket)
	}
	if indexed != len(secretPrefixes) {
		t.Fatalf("index holds %d prefixes, secretPrefixes has %d — index is not a complete partition", indexed, len(secretPrefixes))
	}

	for _, sp := range secretPrefixes {
		if len(sp.prefix) == 0 {
			continue
		}
		bucket := secretPrefixesByFirstByte[sp.prefix[0]]
		found := false
		for _, got := range bucket {
			if got.prefix == sp.prefix && got.minTail == sp.minTail {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("prefix %q (first byte %q) is missing from its index bucket — RedactSecrets would never probe it", sp.prefix, string(sp.prefix[0]))
		}
	}

	// Longest-match-wins ordering within a bucket must be preserved: in the 's'
	// bucket "sk-ant-"/"sk-proj-" must precede the bare "sk-" fallback so a
	// `sk-ant-…` token is not swallowed by the shorter prefix first.
	sBucket := secretPrefixesByFirstByte['s']
	posOf := func(p string) int {
		for i, sp := range sBucket {
			if sp.prefix == p {
				return i
			}
		}
		return -1
	}
	skAnt, skProj, sk := posOf("sk-ant-"), posOf("sk-proj-"), posOf("sk-")
	if skAnt < 0 || skProj < 0 || sk < 0 {
		t.Fatalf("expected sk-ant-/sk-proj-/sk- all in 's' bucket; got positions %d/%d/%d", skAnt, skProj, sk)
	}
	if !(skAnt < sk && skProj < sk) {
		t.Errorf("longest-match ordering broken in 's' bucket: sk-ant-@%d, sk-proj-@%d must precede sk-@%d", skAnt, skProj, sk)
	}
}
