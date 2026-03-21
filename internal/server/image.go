package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// imagePathRe matches absolute file paths ending in common image extensions.
var imagePathRe = regexp.MustCompile(`(/\S+\.(?:png|jpg|jpeg|gif|webp|bmp))`)

// extractImagePaths finds local image file paths in text that actually exist on disk.
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
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			valid = append(valid, path)
			seen[path] = true
		}
	}
	return valid
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
