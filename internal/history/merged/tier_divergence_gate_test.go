package merged

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Cross-tier divergence gates (F4 #2663).
//
// merged dedups across tiers by CONTENT, not by a shared key: the two tiers'
// UUIDs never coincide by construction (local stamps crypto/rand, the Claude CLI
// stamps its own — verified in #2646 that naozhi's uuid appears nowhere in the
// CLI's on-disk transcript). So every time the two tiers render the SAME message
// differently in a way contentKey has not normalised, a user sees the message
// twice.
//
// That has happened three times in production, each fixed by adding another
// normalisation rule:
//
//	fdfd9762 (#2406)  local appended " [+N image(s)]" to Summary → Summary left out of the key
//	09ca3c3e          both tiers emit Detail=="" for image-only → key abstained on both sides
//	869e8802          local caps Detail at 2000 runes, fallback at 16000 → keys diverged at rune 2000
//
// The 40 tests that existed pinned the resulting BEHAVIOUR (pairing cardinality,
// skew direction, order independence). None pinned the PREMISE those behaviours
// rest on: that the two tiers agree about the same message in the first place.
// These do.

// TestDetailCapsOrdering pins the relationship 869e8802's fix depends on.
//
// contentKey normalises Detail to the live tier's cap. That is only sound while
// the live cap is the TIGHTER of the two: TruncateRunes(TruncateRunes(s, 16000),
// 2000) == TruncateRunes(s, 2000) holds because 2000 <= 16000. Invert them and
// the normalisation silently stops making the keys equal — the fix would still
// compile, still run, and long prompts would render twice again.
//
// The relationship is documented in clievent/event.go's comment on
// EventDetailMaxRunes. A comment is not a gate.
func TestDetailCapsOrdering(t *testing.T) {
	if clievent.EventDetailMaxRunes > history.DetailMaxRunes {
		t.Fatalf("live cap (clievent.EventDetailMaxRunes=%d) exceeds the fallback cap "+
			"(history.DetailMaxRunes=%d). contentKey normalises Detail to the live cap, "+
			"which only equalises the two tiers while the live cap is the tighter one — "+
			"869e8802's fix depends on it.",
			clievent.EventDetailMaxRunes, history.DetailMaxRunes)
	}
}

// TestContentKeyMatchesAcrossTierDetailCaps is the reconciliation itself: the
// same source text, truncated the way each tier truncates it, must produce ONE
// contentKey.
//
// Both sides are built from the real caps rather than literals, so changing
// either constant fails here instead of in a bug report. The text lengths bracket
// the live cap: below it the two tiers store identical Detail, above it they
// diverge and the normalisation has to do the work.
func TestContentKeyMatchesAcrossTierDetailCaps(t *testing.T) {
	cases := []struct {
		name  string
		runes int
	}{
		{"well_below_live_cap", 50},
		{"just_below_live_cap", clievent.EventDetailMaxRunes - 1},
		{"exactly_live_cap", clievent.EventDetailMaxRunes},
		{"just_above_live_cap", clievent.EventDetailMaxRunes + 1},
		{"far_above_live_cap", clievent.EventDetailMaxRunes + 5000},
		{"at_fallback_cap", history.DetailMaxRunes},
		{"above_fallback_cap", history.DetailMaxRunes + 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A multi-byte body so a byte-vs-rune slip shows up as a mismatch
			// rather than passing by accident on ASCII.
			text := strings.Repeat("界", c.runes)

			// Fallback tier: the real constructor every external-transcript
			// reader goes through.
			fb := history.NewDerivedEntry(1_700_000_000_000, "user", text)

			// Live tier: the same field the live path stores (cli.buildUserEntry
			// truncates to exactly this cap; the cli-side gate in
			// tier_detail_cap_gate_test.go pins that constructor to this rule so
			// this test does not have to reach into an unexported function).
			local := clievent.EventEntry{
				Type:   "user",
				Detail: textutil.TruncateRunes(text, clievent.EventDetailMaxRunes),
			}

			if got, want := contentKey(local), contentKey(fb); got != want {
				t.Errorf("contentKey diverged across tiers for a %d-rune message:\n local  %q\n fallback %q\n"+
					"the two tiers truncate the same text at different caps; contentKey must normalise them",
					c.runes, got, want)
			}
		})
	}
}

// TestContentKeyNeverAbstainsWithImages pins 09ca3c3e's root cause. An image-only
// user message has Detail=="" on BOTH tiers, so a Detail-only key abstained on
// both sides, no UUID ever matched, and the message survived twice.
//
// Abstention itself is legitimate — an entry with neither text nor images has no
// content to key on. What must never happen is abstaining on an entry that DOES
// carry content.
func TestContentKeyNeverAbstainsWithImages(t *testing.T) {
	cases := []clievent.EventEntry{
		{Type: "user", Detail: "", Images: []string{"data:image/jpeg;base64,AAAA"}},
		{Type: "user", Detail: "", Images: []string{"data:image/jpeg;base64,AAAA", "data:image/jpeg;base64,BBBB"}},
		{Type: "user", Detail: "text and image", Images: []string{"data:image/jpeg;base64,AAAA"}},
	}
	for _, e := range cases {
		if contentKey(e) == "" {
			t.Errorf("contentKey abstained on an entry carrying %d image(s) and Detail=%q; "+
				"both tiers emit Detail==\"\" for image-only messages, so an abstention here "+
				"means neither dedup branch can fire and the message renders twice (09ca3c3e)",
				len(e.Images), e.Detail)
		}
	}
	// The one case where abstaining IS correct, stated so the assertion above
	// cannot be "fixed" by making contentKey never abstain at all.
	if got := contentKey(clievent.EventEntry{Type: "user"}); got != "" {
		t.Errorf("contentKey(no detail, no images) = %q, want abstention: an entry with no "+
			"content must dedup by UUID only, or unrelated empty entries collapse together", got)
	}
}

// TestContentKeyImageIdentityDependsOnIdenticalThumbnails states what makes the
// image key work at all: both tiers must produce byte-identical data URIs. The
// shared clievent.ThumbMaxDim is what guarantees the maxDim half of that (it
// replaced two bare 600s in cli and one same-valued constant in discovery whose
// doc only CLAIMED they matched). This asserts the consequence — differing
// thumbnails must NOT collapse — so the key stays a real discriminator.
func TestContentKeyImageIdentityDependsOnIdenticalThumbnails(t *testing.T) {
	a := clievent.EventEntry{Type: "user", Images: []string{"data:image/jpeg;base64,AAAA"}}
	b := clievent.EventEntry{Type: "user", Images: []string{"data:image/jpeg;base64,BBBB"}}
	if contentKey(a) == contentKey(b) {
		t.Error("two different thumbnails produced the same contentKey; the image identity " +
			"must discriminate, or distinct image messages collapse into one")
	}
	// Same bytes → same key, which is the property that makes cross-tier image
	// dedup possible once both tiers use clievent.ThumbMaxDim.
	if contentKey(a) != contentKey(clievent.EventEntry{Type: "user", Images: []string{"data:image/jpeg;base64,AAAA"}}) {
		t.Error("identical thumbnails produced different contentKeys; cross-tier image dedup " +
			"depends on this being deterministic")
	}
}
