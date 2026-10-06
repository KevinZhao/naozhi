package attachment

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/osutil"
)

// Errors ClassifyFile returns; callers map them to a user-facing reason.
var (
	ErrUnsupportedType = errors.New("attachment: unsupported file type")
	ErrTooLarge        = errors.New("attachment: file too large")
)

// textFileMime maps each accepted text extension to its MIME type.
var textFileMime = map[string]string{
	".txt":  "text/plain",
	".log":  "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
	".json": "application/json",
	".yaml": "application/yaml",
	".yml":  "application/yaml",
}

// ClassifyFile decides whether a document the user sent can be handed to the
// model as a workspace file and returns its MIME type. A PDF is recognised by
// its %PDF- magic, whatever its name; anything else must carry a text
// extension from textFileMime and be valid UTF-8 without NUL bytes, so a
// binary renamed to .txt is refused. Archives (gzip, zip, hence docx/xlsx)
// are refused outright.
func ClassifyFile(name string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrEmptyData
	}
	if len(data) > limits.MaxFileAttachmentBytes {
		return "", ErrTooLarge
	}
	if bytes.HasPrefix(data, []byte{0x1F, 0x8B}) || bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return "", ErrUnsupportedType
	}
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		return "application/pdf", nil
	}
	// The raw name, not SanitizeOrigName's: its rune cap can cut the extension.
	mime, ok := textFileMime[strings.ToLower(filepath.Ext(name))]
	if !ok || bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return "", ErrUnsupportedType
	}
	return mime, nil
}

// MaybeSupported reports whether an upload with this name or declared MIME
// type could pass ClassifyFile, so an adapter can refuse other uploads
// without downloading them. ClassifyFile still decides on the bytes; a PDF
// with neither a .pdf name nor an application/pdf type is refused here.
func MaybeSupported(name, mime string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	if _, ok := textFileMime[ext]; ok || ext == ".pdf" {
		return true
	}
	base, _, _ := strings.Cut(mime, ";")
	return strings.EqualFold(strings.TrimSpace(base), "application/pdf")
}

// ExtForMime is the on-disk extension Persist accepts for a MIME type
// ClassifyFile returns; "" for any other type.
func ExtForMime(mime string) string {
	switch mime {
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	case "application/yaml":
		return ".yaml"
	default:
		return ""
	}
}

// maxOrigNameRunes caps SanitizeOrigName output so a huge filename cannot
// bloat the prompt or the .meta sidecar.
const maxOrigNameRunes = 120

// SanitizeOrigName makes a user-supplied filename safe to embed in the .meta
// sidecar, Content-Disposition headers and the Read-tool hint: control and
// bidi-override runes are dropped, '/' and '\\' become '_' (filepath.Base
// would miss Windows separators on Linux), and the result is capped at
// maxOrigNameRunes. It is never a path component.
func SanitizeOrigName(name string) string {
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop C0 control chars
		case osutil.IsLogInjectionRune(r):
			// drop C1 controls and bidi-override runes
		case r == '/' || r == '\\':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	// Byte short-circuit: ≤ N bytes implies ≤ N runes.
	if len(out) > maxOrigNameRunes && utf8.RuneCountInString(out) > maxOrigNameRunes {
		runes := []rune(out)
		out = string(runes[:maxOrigNameRunes])
	}
	return out
}
