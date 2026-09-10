package project

import (
	"net/http"
	"path/filepath"
	"strings"
)

// textMimeSet lists non-"text/" MIME types safe to return as UTF-8 text in
// preview mode (http.DetectContentType may return these specific types).
var textMimeSet = map[string]struct{}{
	"application/json":          {},
	"application/javascript":    {},
	"application/typescript":    {},
	"application/xml":           {},
	"application/x-yaml":        {},
	"application/yaml":          {},
	"application/toml":          {},
	"application/x-sh":          {},
	"application/x-shellscript": {},
}

// previewableByExt overrides the application/octet-stream that
// DetectContentType returns for most source extensions.
var previewableByExt = map[string]string{
	".go":       "text/x-go",
	".py":       "text/x-python",
	".js":       "application/javascript",
	".mjs":      "application/javascript",
	".ts":       "application/typescript",
	".tsx":      "application/typescript",
	".jsx":      "application/javascript",
	".rs":       "text/x-rust",
	".java":     "text/x-java",
	".kt":       "text/x-kotlin",
	".kts":      "text/x-kotlin",
	".c":        "text/x-c",
	".h":        "text/x-c",
	".cc":       "text/x-c++",
	".cpp":      "text/x-c++",
	".hpp":      "text/x-c++",
	".cs":       "text/x-csharp",
	".rb":       "text/x-ruby",
	".php":      "text/x-php",
	".swift":    "text/x-swift",
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".log":      "text/plain",
	".json":     "application/json",
	".jsonl":    "application/json",
	".yaml":     "application/yaml",
	".yml":      "application/yaml",
	".toml":     "application/toml",
	".xml":      "application/xml",
	// .html / .htm intentionally NOT mapped: servePreview/serveRaw block
	// text/html after sniffing, and listing them here would let
	// mimeFromExtOnly's fast path return text/html without the sniff.
	".css":        "text/css",
	".sh":         "application/x-sh",
	".bash":       "application/x-sh",
	".zsh":        "application/x-sh",
	".sql":        "text/x-sql",
	".dockerfile": "text/plain",
	// .env intentionally NOT mapped: it falls through to DetectContentType as
	// application/octet-stream so servePreview's MIME guard rejects it.
	".gitignore":     "text/plain",
	".gitattributes": "text/plain",
	".makefile":      "text/x-makefile",
	".mk":            "text/x-makefile",
	".proto":         "text/x-protobuf",
	".graphql":       "text/plain",
	".gql":           "text/plain",
	// .conf / .cfg / .ini are deliberately previewable: authenticated users
	// already have full read access (download / raw / CLI Read), so refusing
	// preview only adds click-through cost. Secret-name blocking belongs in
	// sensitiveDownloadNames / sensitiveDownloadExts, not here.
	".conf": "text/plain",
	".cfg":  "text/plain",
	".ini":  "text/plain",
}

// rawPreviewMimes lists types the browser may render inline via <img> or
// <iframe> under mode=raw. SVG is intentionally absent: serveRaw rejects
// image/svg+xml (stored XSS via <script>), and SVG previews flow only through
// serveRender's blob-URL path.
var rawPreviewMimes = []string{
	"image/png", "image/jpeg", "image/gif", "image/webp",
	"application/pdf",
}

// detectMime runs http.DetectContentType on the first 512 bytes plus an
// extension override for source code that would otherwise be tagged as
// application/octet-stream.
func detectMime(resolved string, head []byte) string {
	mime := http.DetectContentType(head)
	ext := strings.ToLower(filepath.Ext(resolved))
	// SVGs starting with `<?xml ?>` sniff as text/xml, which isTextMime
	// accepts and would bypass serveRaw's image/svg+xml block. Pin .svg so
	// serveRaw's attachment disposition always forces a download.
	if ext == ".svg" {
		return "image/svg+xml"
	}
	// Pin .html / .htm to text/html here ONLY (not in previewableByExt) so
	// serveRender can route empty/short HTML that sniffs as text/plain, while
	// mimeFromExtOnly's fast path can never short-circuit the byte sniff and
	// servePreview / serveRaw still hit their text/html block gates.
	if ext == ".html" || ext == ".htm" {
		if strings.HasPrefix(mime, "text/plain") || strings.HasPrefix(mime, "application/octet-stream") {
			return "text/html"
		}
		return mime
	}
	// Base name override for extensionless files (Dockerfile / Makefile).
	// Dotfiles like ".gitignore" have filepath.Ext == basename, so look them
	// up by basename directly.
	if ext == "" {
		base := strings.ToLower(filepath.Base(resolved))
		if v, ok := previewableByExt["."+base]; ok {
			return v
		}
	} else if base := strings.ToLower(filepath.Base(resolved)); strings.HasPrefix(base, ".") && base == ext {
		if v, ok := previewableByExt[base]; ok {
			return v
		}
	}
	if strings.HasPrefix(mime, "text/plain") || strings.HasPrefix(mime, "application/octet-stream") {
		if v, ok := previewableByExt[ext]; ok {
			return v
		}
	}
	return mime
}
func isTextMime(mime string) bool {
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	base := mime
	if i := strings.Index(mime, ";"); i > 0 {
		base = strings.TrimSpace(mime[:i])
	}
	_, ok := textMimeSet[base]
	return ok
}
func isRawPreviewMime(mime string) bool {
	base := mime
	if i := strings.Index(mime, ";"); i > 0 {
		base = strings.TrimSpace(mime[:i])
	}
	for _, p := range rawPreviewMimes {
		if base == p {
			return true
		}
	}
	return false
}

// mimeFromExtOnly returns the extension-derived MIME when the path alone
// unambiguously resolves it, so statRelWithRoot's batch path can skip the
// sniff. Returns ok only when the sniff would yield the same answer: .svg is
// pinned regardless of bytes, and previewableByExt entries are what the sniff
// path itself falls back to. Empty extensions and binary formats fall through.
func mimeFromExtOnly(resolved string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(resolved))
	if ext == ".svg" {
		return "image/svg+xml", true
	}
	if ext == "" {
		// Extensionless files need basename lookup; defer to detectMime.
		return "", false
	}
	if v, ok := previewableByExt[ext]; ok {
		return v, true
	}
	return "", false
}
