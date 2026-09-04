// "上传字节 → cli.Attachment" 验证流水线：parseAttachmentFile（magic-byte
// sniff + size gate）、pdfNestedInImage（防 JFIF+PDF 嵌套）、
// hasPersistableAttachment、imageExtForMime、sanitizeClientFilename。
// maxImageBytes / maxPDFBytes / uploadBodyBytes 定义在 dashboard_send.go。
package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/osutil"
)

// parseAttachmentFile reads a multipart file header and returns the
// classified cli.Attachment. Only JPEG/PNG/GIF/WebP raster images and PDFs
// are accepted, classified by magic-byte sniff (never the client
// Content-Type); anything else is rejected before persistence (#886).
// Images become KindImageInline; PDFs become KindFileRef with bytes still in
// Data for the caller to persist once the workspace is known.
//
// allowPDF is false on the inline-multipart /api/sessions/send path, whose
// body cap fits images only; the upload-only endpoint passes true.
func parseAttachmentFile(fh *multipart.FileHeader, allowPDF bool) (cli.Attachment, error) {
	declared := fh.Header.Get("Content-Type")
	// declared is client-controlled and only picks the size gate; the
	// magic-byte sniff below is the authority on type.
	isPDF := declared == "application/pdf"
	if isPDF && !allowPDF {
		return cli.Attachment{}, fmt.Errorf("PDF attachments must be sent via /api/sessions/upload")
	}

	// Refuse oversize on metadata alone before pulling the body into memory.
	switch {
	case isPDF:
		if fh.Size > maxPDFBytes {
			return cli.Attachment{}, fmt.Errorf("PDF too large (max %d MB)", maxPDFBytes>>20)
		}
	default:
		if fh.Size > maxImageBytes {
			return cli.Attachment{}, fmt.Errorf("file too large (max %d MB)", maxImageBytes>>20)
		}
	}

	f, err := fh.Open()
	if err != nil {
		// Generic client message: os.PathError could leak the temp-file path.
		slog.Debug("upload: open multipart file failed", "err", err)
		return cli.Attachment{}, errors.New("failed to read uploaded file")
	}
	defer f.Close()

	// Sniff a 512-byte head (http.DetectContentType's maximum) BEFORE
	// buffering: a declared-PDF body without %PDF- magic must be rejected
	// before it can allocate maxPDFBytes (per-request memory DoS, #503). The
	// head is re-joined via io.MultiReader below.
	const sniffLen = 512
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(f, head)
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		head = head[:n]
	default:
		slog.Debug("upload: head read failed", "err", err)
		return cli.Attachment{}, errors.New("failed to read uploaded file")
	}

	// fh.Size is client-controlled and may be understated, so the LimitReader
	// (+1 to detect overflow) is the real byte cap.
	headLooksPDF := len(head) >= 5 && bytes.Equal(head[:5], []byte("%PDF-"))
	var sizeLimit int64
	switch {
	case isPDF && headLooksPDF:
		sizeLimit = maxPDFBytes
	case isPDF && !headLooksPDF:
		// A declared PDF without the magic header cannot be legitimate;
		// reject before allocating up to maxPDFBytes.
		return cli.Attachment{}, fmt.Errorf("file does not look like a PDF")
	default:
		sizeLimit = maxImageBytes
	}
	body := io.MultiReader(bytes.NewReader(head), f)
	data, err := io.ReadAll(io.LimitReader(body, sizeLimit+1))
	if err != nil {
		slog.Debug("upload: read multipart file failed", "err", err)
		return cli.Attachment{}, errors.New("failed to read uploaded file")
	}
	if int64(len(data)) > sizeLimit {
		return cli.Attachment{}, fmt.Errorf("file too large (max %d MB)", sizeLimit>>20)
	}

	// Reject gzip magic explicitly so no downstream component can ever
	// accept a compressed container (bomb) for an attachment.
	if len(data) >= 2 && data[0] == 0x1F && data[1] == 0x8B {
		return cli.Attachment{}, fmt.Errorf("compressed files are not accepted")
	}
	detected := http.DetectContentType(data)
	if isPDF {
		// Declared PDF that does not sniff as PDF is spoofed or corrupt.
		if detected != "application/pdf" {
			return cli.Attachment{}, fmt.Errorf("file does not look like a PDF")
		}
		return cli.Attachment{
			Kind:     cli.KindFileRef,
			Data:     data,
			MimeType: "application/pdf",
			OrigName: sanitizeClientFilename(fh.Filename),
			Size:     int64(len(data)),
		}, nil
	}

	// Image path gates purely on the sniffed MIME with an explicit raster
	// allowlist, so a sniffer change can never let SVG (text/xml) or another
	// oddity through (#886).
	switch detected {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		// ok
	default:
		return cli.Attachment{}, fmt.Errorf("only image/* or application/pdf files are accepted")
	}
	// DetectContentType only inspects leading bytes; an image header followed
	// by an embedded PDF body must not be persisted as KindImageInline (#1002).
	if pdfNestedInImage(data) {
		return cli.Attachment{}, fmt.Errorf("file appears to be a PDF disguised as an image")
	}
	return cli.Attachment{
		Kind:     cli.KindImageInline,
		Data:     data,
		MimeType: detected,
	}, nil
}

// pdfNestedInImage reports whether the payload — already classified as an
// image by http.DetectContentType — contains a "%PDF-" magic sequence
// anywhere in the buffer. It must scan the FULL buffer, not a fixed window:
// a crafted JFIF APPn/ICC segment can legally push "%PDF-" past any window
// (#1890). The caller bounds the buffer to maxImageBytes, and legitimate
// images never contain "%PDF-".
func pdfNestedInImage(data []byte) bool {
	return bytes.Contains(data, pdfMagicSignature)
}

// pdfMagicSignature is the 5-byte signature every conforming PDF starts with.
var pdfMagicSignature = []byte("%PDF-")

// hasPersistableAttachment reports whether any attachment needs to hit
// persistFileRefs (file_ref must land on disk; inline images are persisted
// best-effort for the lightbox). Otherwise the caller skips workspace
// resolution + persist entirely.
func hasPersistableAttachment(atts []cli.Attachment) bool {
	for _, a := range atts {
		if a.Kind == cli.KindFileRef {
			return true
		}
		if imageExtForMime(a.MimeType) != "" && len(a.Data) > 0 {
			return true
		}
	}
	return false
}

// imageExtForMime maps a recognised image MIME type to its canonical file
// extension (with the leading dot); "" for anything outside the allowlist,
// which matches attachment.sanitizeExt.
func imageExtForMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

// sanitizeClientFilename strips control characters and path separators
// from a multipart filename so it is safe to embed in the .meta sidecar,
// Content-Disposition headers, and the text hint Claude receives, and caps
// it at maxClientFilenameRunes. The filename is fully client-controlled and
// must never be trusted as a path component; both '/' and '\\' are
// collapsed to '_' (filepath.Base would miss Windows separators on Linux).
func sanitizeClientFilename(name string) string {
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
	if len(out) > maxClientFilenameRunes && utf8.RuneCountInString(out) > maxClientFilenameRunes {
		runes := []rune(out)
		out = string(runes[:maxClientFilenameRunes])
	}
	return out
}

// maxClientFilenameRunes caps sanitizeClientFilename output so a huge
// filename cannot bloat the prompt or the .meta sidecar.
const maxClientFilenameRunes = 120
