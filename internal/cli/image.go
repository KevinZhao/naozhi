package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxImageFileSize = 10 * 1024 * 1024 // 10MB

// safeImageDirs are the only directory prefixes from which image files may be read.
// This prevents prompt-injection attacks that trick the CLI into emitting arbitrary paths.
var safeImageDirs = []string{"/tmp/"}

// imagePathRe matches absolute file paths ending in common image extensions.
var imagePathRe = regexp.MustCompile(`(/\S+\.(?:png|jpg|jpeg|gif|webp|bmp))`)

// ExtractImagePaths finds local image file paths in text that actually exist on disk.
// Only paths under safe directories (e.g., /tmp) are returned.
func ExtractImagePaths(text string) []string {
	matches := imagePathRe.FindAllString(text, 10) // cap at 10 images
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
