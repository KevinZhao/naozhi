package cli

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/textutil"
)

// The live half of the cross-tier reconciliation (F4 #2663).
//
// merged.contentKey normalises a fallback entry's Detail to
// clievent.EventDetailMaxRunes before comparing it with the live tier's. That is
// only equal to the live tier's Detail if buildUserEntry truncates to exactly
// that cap, by exactly that rule. The merged-side gate
// (tier_divergence_gate_test.go) asserts the keys match; it builds the live side
// from textutil.TruncateRunes rather than reaching into this unexported
// constructor, so this test is what ties the real constructor to that rule.
//
// 869e8802 was the production bug: local capped at 2000, every fallback reader at
// 16000, contentKey compared verbatim, and any prompt over 2000 runes rendered
// twice whenever a LoadBefore page overlapped the live window.

// TestBuildUserEntryDetailUsesTheSharedCap pins that the live tier truncates
// Detail to clievent.EventDetailMaxRunes and to nothing else — no suffix, no
// second cap, no byte-based slice.
func TestBuildUserEntryDetailUsesTheSharedCap(t *testing.T) {
	for _, runes := range []int{
		10,
		clievent.EventDetailMaxRunes - 1,
		clievent.EventDetailMaxRunes,
		clievent.EventDetailMaxRunes + 1,
		clievent.EventDetailMaxRunes + 5000,
	} {
		// Multi-byte so a byte-vs-rune slip is visible.
		text := strings.Repeat("界", runes)
		got := buildUserEntry(text, nil).Detail
		want := textutil.TruncateRunes(text, clievent.EventDetailMaxRunes)
		if got != want {
			t.Errorf("buildUserEntry(%d runes).Detail is not TruncateRunes(text, %d):\n got %d runes\nwant %d runes\n"+
				"merged.contentKey normalises the fallback tier to this exact rule; any other "+
				"transformation here makes the two tiers' keys diverge",
				runes, clievent.EventDetailMaxRunes,
				len([]rune(got)), len([]rune(want)))
		}
	}
}

// TestLiveAndFallbackDetailAgreeThroughTheLiveCap is the reconciliation across
// the REAL constructors on both sides: buildUserEntry here, and
// history.NewDerivedEntry (what every external-transcript reader goes through)
// there. Truncating the fallback Detail to the live cap must reproduce the live
// Detail byte for byte.
func TestLiveAndFallbackDetailAgreeThroughTheLiveCap(t *testing.T) {
	for _, runes := range []int{
		50,
		clievent.EventDetailMaxRunes,
		clievent.EventDetailMaxRunes + 1,
		history.DetailMaxRunes,
		history.DetailMaxRunes + 500,
	} {
		text := strings.Repeat("界", runes)
		live := buildUserEntry(text, nil).Detail
		fallback := history.NewDerivedEntry(1_700_000_000_000, "user", text).Detail
		if got := textutil.TruncateRunes(fallback, clievent.EventDetailMaxRunes); got != live {
			t.Errorf("tiers disagree for a %d-rune message: normalising the fallback Detail to "+
				"the live cap gave %d runes, live stored %d",
				runes, len([]rune(got)), len([]rune(live)))
		}
	}
}

// TestBuildUserEntrySummaryCarriesTheImageSuffix documents why Summary is NOT
// part of contentKey (fdfd9762 / #2406): the live tier decorates it and the
// fallback tier does not. Asserted rather than commented so that if the
// decoration is ever removed, whoever removes it learns that contentKey's
// exclusion of Summary was compensating for it and can reconsider.
func TestBuildUserEntrySummaryCarriesTheImageSuffix(t *testing.T) {
	plain := buildUserEntry("hello", nil).Summary
	withImg := buildUserEntry("hello", []clievent.Attachment{
		{Kind: clievent.KindImageInline, Data: []byte("not-an-image"), MimeType: "image/png"},
	}).Summary
	if plain == withImg {
		t.Fatal("Summary is identical with and without images; the live tier used to append " +
			"\" [+N image(s)]\", which is why merged.contentKey excludes Summary (#2406). " +
			"If the decoration is gone, revisit that exclusion.")
	}
	if !strings.Contains(withImg, "image(s)]") {
		t.Errorf("Summary with images = %q, expected the \" [+N image(s)]\" decoration", withImg)
	}
}

// TestThumbnailDimIsShared pins that the live path passes the shared constant to
// MakeThumbnail rather than a literal. discovery's rehydration path uses the same
// constant; when they diverge, image-only messages render twice (09ca3c3e),
// because contentKey identifies them by a hash over the thumbnail bytes.
//
// The fixture is deliberately LARGER than any plausible maxDim. A small image is
// returned unscaled at every dim, so a 1x1 fixture cannot distinguish 600 from
// 800 — the first version of this test used one and passed even with the dims
// deliberately mismatched, i.e. it asserted nothing.
func TestThumbnailDimIsShared(t *testing.T) {
	png := gradientPNG(t, clievent.ThumbMaxDim+400)

	live := buildUserEntry("", []clievent.Attachment{
		{Kind: clievent.KindImageInline, Data: png, MimeType: "image/png"},
	}).Images
	if len(live) != 1 {
		t.Fatalf("expected one thumbnail, got %d", len(live))
	}
	if want := MakeThumbnail(png, clievent.ThumbMaxDim); live[0] != want {
		t.Errorf("live thumbnail was not derived at clievent.ThumbMaxDim (%d).\n"+
			"discovery's rehydration path uses that constant; a different dim here yields "+
			"different bytes, contentKey's image identity stops matching, and every image-only "+
			"message renders twice (09ca3c3e).", clievent.ThumbMaxDim)
	}
	// The fixture must actually be dim-sensitive, or the assertion above is
	// vacuous however it is wired.
	if MakeThumbnail(png, clievent.ThumbMaxDim) == MakeThumbnail(png, clievent.ThumbMaxDim+200) {
		t.Error("the fixture produces identical thumbnails at two different maxDims; this test " +
			"cannot detect a dim divergence and needs a larger image")
	}
}

// gradientPNG builds a dim x dim PNG whose pixels vary, so rescaling it to
// different long edges yields different JPEG bytes.
func gradientPNG(t *testing.T, dim int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, dim, dim))
	for y := 0; y < dim; y++ {
		for x := 0; x < dim; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}
