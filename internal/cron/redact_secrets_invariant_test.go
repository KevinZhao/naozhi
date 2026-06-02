package cron

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
