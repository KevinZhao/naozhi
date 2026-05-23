package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxImageFileSize = 10 * 1024 * 1024 // 10MB

// maxExtractedImages caps how many image-path matches ExtractImagePaths
// returns from a single text scan. Bounding the regex's FindAllString
// match count protects the per-message stat() / EvalSymlinks loop from
// linear amplification on a hostile (or accidentally noisy) tool_result
// payload that splatters dozens of fake-looking paths into output. 10
// covers any realistic burst of inline images a single CLI turn would
// emit; messages above that cap fall back to the leading 10 in document
// order, which is the same de-facto policy the dashboard renders today.
const maxExtractedImages = 10

// safeImageDirs are the only directory prefixes from which image files may be read.
// This prevents prompt-injection attacks that trick the CLI into emitting arbitrary paths.
//
// Trailing-slash invariant: every entry MUST end with `/` so a prefix
// match cannot accept a confusable sibling directory (e.g. `/tmpfoo/...`
// when the allowlist contained the bare `/tmp`). isUnderSafeDir relies
// on this for correctness — adding a new dir without the trailing slash
// would silently widen the allowlist.
var safeImageDirs = []string{"/tmp/"}

// imagePathRe matches absolute file paths ending in common image extensions.
var imagePathRe = regexp.MustCompile(`(/\S+\.(?:png|jpg|jpeg|gif|webp|bmp))`)

// ExtractImagePaths finds local image file paths in text that actually exist on disk.
// Only paths under safe directories (e.g., /tmp) are returned.
func ExtractImagePaths(text string) []string {
	matches := imagePathRe.FindAllString(text, maxExtractedImages)
	var valid []string
	seen := make(map[string]bool)
	for _, path := range matches {
		// Clean trailing punctuation that regex may capture
		path = strings.TrimRight(path, ".,;:!?)]}>\"'")
		// Resolve symlinks and canonicalize to prevent traversal
		cleaned, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if seen[cleaned] {
			continue
		}
		if !isUnderSafeDir(cleaned) {
			continue
		}
		info, err := os.Stat(cleaned)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Size() > maxImageFileSize {
			continue
		}
		valid = append(valid, cleaned)
		seen[cleaned] = true
	}
	return valid
}

func isUnderSafeDir(path string) bool {
	for _, dir := range safeImageDirs {
		if strings.HasPrefix(path, dir) {
			return true
		}
	}
	return false
}

// MimeFromPath returns a MIME type based on file extension.
//
// R228-ARCH-5 archive anchor: this function is the inverse of
// `platform.ImageExt(mime) string` — they together form a small but
// genuinely two-way mapping (path→mime here; mime→ext over there).
// A previous review proposed extracting both into a shared
// `internal/imageutil` package. Decision: keep them in their current
// owners. cli.MimeFromPath is consumed by cli's own `dispatch.go` image
// pipeline (the only caller passes paths produced by ExtractImagePaths
// above, so MIME inference is a CLI-protocol concern). platform.ImageExt
// names files for outbound platform uploads (filename extension is a
// presentation concern in the channel adapter). The two surfaces happen
// to share a 5-entry switch but evolve under different drivers — adding
// HEIC support, for instance, depends on whether the change is "Claude
// CLI now emits .heic in tool_use" (here) vs "Feishu accepts image/heic
// in upload API" (there). Co-locating them under imageutil would couple
// the two evolution axes for a savings of <30 lines. The duplication is
// acknowledged and accepted; consumers must update both sites when
// extending the supported MIME set.
func MimeFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return "image/png"
	}
}
