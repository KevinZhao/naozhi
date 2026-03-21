package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxImageFileSize = 10 * 1024 * 1024 // 10MB

// safeImageDirs are the only directory prefixes from which image files may be read.
// This prevents prompt-injection attacks that trick the CLI into emitting arbitrary paths.
var safeImageDirs = []string{"/tmp/", os.TempDir() + "/"}

// imagePathRe matches absolute file paths ending in common image extensions.
var imagePathRe = regexp.MustCompile(`(/\S+\.(?:png|jpg|jpeg|gif|webp|bmp))`)

// extractImagePaths finds local image file paths in text that actually exist on disk.
// Only paths under safe directories (e.g., /tmp) are returned.
func extractImagePaths(text string) []string {
	matches := imagePathRe.FindAllString(text, 10) // cap at 10 images
	var valid []string
	seen := make(map[string]bool)
	for _, path := range matches {
		// Clean trailing punctuation that regex may capture
		path = strings.TrimRight(path, ".,;:!?)]}>\"'")
		if seen[path] {
			continue
		}
		// Resolve to absolute canonical path to prevent traversal
		cleaned := filepath.Clean(path)
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
		seen[path] = true
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

// mimeFromPath returns a MIME type based on file extension.
func mimeFromPath(path string) string {
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

// stripMIMEParams extracts the bare media type from a Content-Type value
// that may contain parameters (e.g., "image/png; name=file.png" → "image/png").
func stripMIMEParams(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct)
}
