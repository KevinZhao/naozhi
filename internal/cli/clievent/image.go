// image.go — image-path extraction and MIME sniffing for user text (#2649 G1-e).
//
// Moved out of internal/cli: 80 lines importing nothing but stdlib, consumed by
// internal/dispatch, and about the shape of an Attachment — which lives here.
package clievent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const maxImageFileSize = 10 * 1024 * 1024 // 10MB

// safeImageDirs are the only directory prefixes from which image files may be read.
// This prevents prompt-injection attacks that trick the CLI into emitting arbitrary paths.
// Each also appears symlink-resolved, since candidates are compared after
// EvalSymlinks: on macOS /tmp is a link to /private/tmp. Resolved on first
// use rather than at package init.
var safeImageDirs = sync.OnceValue(func() []string { return resolveSafeDirs([]string{"/tmp/"}) })

// imagePathRe matches absolute file paths ending in the image extensions the
// model accepts (png/jpeg/gif/webp, as platform.SupportedImageMIME).
var imagePathRe = regexp.MustCompile(`(/\S+\.(?:png|jpg|jpeg|gif|webp))`)

// ImagePath is an image path found in text by ExtractImagePaths.
type ImagePath struct {
	// Text is the path as written in the text, for rewriting it there.
	Text string
	// Path is Text with symlinks resolved, the file that passed the safe-dir
	// check; read this one, not Text.
	Path string
}

// ExtractImagePaths finds local image file paths in text that actually exist on disk.
// Only paths under safe directories (e.g., /tmp) are returned.
func ExtractImagePaths(text string) []ImagePath {
	matches := imagePathRe.FindAllString(text, 10) // cap at 10 images
	var valid []ImagePath
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
		valid = append(valid, ImagePath{Text: path, Path: cleaned})
		seen[cleaned] = true
	}
	return valid
}

// resolveSafeDirs returns dirs (each ending in "/") followed by the
// symlink-resolved form of each that differs, also ending in "/" so /tmpfoo
// never matches /tmp. A dir that does not resolve keeps only its literal form.
func resolveSafeDirs(dirs []string) []string {
	out := append([]string(nil), dirs...)
	for _, dir := range dirs {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		resolved = strings.TrimSuffix(resolved, "/") + "/"
		if resolved != dir {
			out = append(out, resolved)
		}
	}
	return out
}

func isUnderSafeDir(path string) bool {
	for _, dir := range safeImageDirs() {
		if strings.HasPrefix(path, dir) {
			return true
		}
	}
	return false
}

// MimeFromPath returns a MIME type based on file extension. It is the
// intentional inverse of platform.ImageExt (kept separate: this side follows
// what the CLI emits, that side what platforms accept); extend both when
// adding a MIME type.
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
	default:
		return "image/png"
	}
}
