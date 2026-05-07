package cli

import (
	"testing"
)

// Index-alignment CRITICAL invariant: sanitizeImagesAligned must never
// let Images[i] and ImagePaths[i] drift out of sync. Replayed history
// (AppendBatch / InjectHistory) may carry untrusted EventEntry values
// where len(ImagePaths) != len(Images); if a valid thumbnail at index j
// loses its corresponding path and the next surviving path silently
// fills that slot, the dashboard lightbox serves the WRONG image's
// original bytes — cross-message confidentiality violation.
func TestSanitizeImagesAligned_KeepsIndicesAligned(t *testing.T) {
	t.Parallel()

	valid := "data:image/png;base64,YWFh"

	t.Run("shorter paths padded with empty", func(t *testing.T) {
		imgs := []string{valid, "junk", valid}
		paths := []string{"a.png"} // deliberately short — replay edge
		gotImgs, gotPaths := sanitizeImagesAligned(imgs, paths)

		if len(gotImgs) != 2 {
			t.Fatalf("imgs len=%d want 2", len(gotImgs))
		}
		if len(gotPaths) != 2 {
			t.Fatalf("paths len=%d want 2 (aligned)", len(gotPaths))
		}
		// Index 0 of the output corresponds to index 0 of the input —
		// paths[0] = "a.png" survives.
		if gotPaths[0] != "a.png" {
			t.Errorf("paths[0]=%q want a.png", gotPaths[0])
		}
		// Index 2 of the input had NO corresponding path; the slot must
		// be "", NOT a leftover from the original paths slice.
		if gotPaths[1] != "" {
			t.Errorf("paths[1]=%q want empty — misalignment would serve wrong file", gotPaths[1])
		}
	})

	t.Run("drop in middle keeps tail aligned", func(t *testing.T) {
		imgs := []string{valid, "garbage", valid}
		paths := []string{"first.png", "bad.png", "third.png"}
		gotImgs, gotPaths := sanitizeImagesAligned(imgs, paths)

		if len(gotImgs) != 2 || len(gotPaths) != 2 {
			t.Fatalf("lens imgs=%d paths=%d want 2/2", len(gotImgs), len(gotPaths))
		}
		// Crucially: surviving entries at input indices {0, 2} must map
		// to paths[0]="first.png" and paths[2]="third.png", NOT to
		// paths[0] + paths[1].
		if gotPaths[0] != "first.png" || gotPaths[1] != "third.png" {
			t.Errorf("paths=%v want [first.png third.png]", gotPaths)
		}
	})

	t.Run("no paths provided returns nil paths", func(t *testing.T) {
		imgs := []string{valid, "junk"}
		gotImgs, gotPaths := sanitizeImagesAligned(imgs, nil)
		if len(gotImgs) != 1 {
			t.Errorf("imgs len=%d want 1", len(gotImgs))
		}
		if gotPaths != nil {
			t.Errorf("paths=%v want nil", gotPaths)
		}
	})

	t.Run("fast path passthrough when all images valid", func(t *testing.T) {
		// When every image is valid, sanitize short-circuits and returns
		// the input paths untouched — the caller already built them in
		// alignment. Contract: no allocation, no filtering.
		imgs := []string{valid, valid}
		paths := []string{"first.png", "second.png"}
		gotImgs, gotPaths := sanitizeImagesAligned(imgs, paths)
		if len(gotImgs) != 2 || len(gotPaths) != 2 {
			t.Fatalf("lens imgs=%d paths=%d want 2/2", len(gotImgs), len(gotPaths))
		}
		if gotPaths[0] != "first.png" || gotPaths[1] != "second.png" {
			t.Errorf("paths=%v want [first.png second.png]", gotPaths)
		}
	})

	t.Run("slow path with all-empty-paths returns nil paths", func(t *testing.T) {
		// Force the slow filter path via an invalid image so
		// sanitizeImagesAligned actually evaluates the `anyPath` gate.
		imgs := []string{valid, "junk", valid}
		paths := []string{"", "", ""}
		_, gotPaths := sanitizeImagesAligned(imgs, paths)
		if gotPaths != nil {
			t.Errorf("paths=%v want nil when none carry data", gotPaths)
		}
	})
}
