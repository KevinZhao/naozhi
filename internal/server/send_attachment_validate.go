// Phase 3f-prep / R-attachment-validate-extract (2026-05-28):
// 5 个 attachment 解析 / 验证 pure func（外加 pdfMagicSignature 与
// maxClientFilenameRunes 这两个仅它们用的常量/变量）抽到独立文件。
// 零行为变化、零跨包搬。
//
// 这五个 func 形成完整的"上传字节 → cli.Attachment"验证流水线：
//  1. parseAttachmentFile  — 主入口，结合 magic-byte sniff + size gate
//  2. pdfNestedInImage     — defence-in-depth，防 JFIF+PDF 嵌套
//  3. hasPersistableAttachment — 调用方决定是否走 persistFileRefs
//  4. imageExtForMime      — MIME → 文件扩展名（与 attachment.sanitizeExt 对齐）
//  5. sanitizeClientFilename — 客户端文件名 → 安全副本（控字符 + 路径分隔符过滤）
//
// 常量 maxImageBytes / maxPDFBytes / uploadBodyBytes 仍保留在 dashboard_send.go
// 的常量块，因为 handleUpload / handleSend 也读它们。本文件的 func 通过
// 同包可见性继续访问，无需任何调用方改动。
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
// classified cli.Attachment. R244-SEC-P2-1 (#886) hardening: only
// JPEG/PNG/GIF/WebP raster images and PDFs (`application/pdf`) are
// accepted; anything else is rejected before persistence.
//
// The returned Kind is one of cli.KindImageInline (raw bytes) or
// cli.KindFileRef (PDF, will be persisted into the session workspace
// before the LLM can read it). Image bytes are persisted on a best-
// effort basis so the dashboard lightbox can show the original; PDFs
// become KindImageInline with raw bytes; PDFs become KindFileRef with the
// bytes still in Data for the caller to later persist to the session
// workspace (the HTTP layer doesn't know which workspace yet when the
// upload-only endpoint is hit). Anything else is rejected.
//
// allowPDF is false for the legacy inline-multipart path of /api/sessions/send:
// that path caps body size at ~22 MB for images only, so letting a 32 MB
// PDF through would trigger a confusing "bad multipart form" error from
// MaxBytesReader instead of a clean "too large" message. The upload-only
// endpoint sets allowPDF=true.
func parseAttachmentFile(fh *multipart.FileHeader, allowPDF bool) (cli.Attachment, error) {
	declared := fh.Header.Get("Content-Type")
	// declared (Content-Type header) is client-controlled and used here only
	// to pick the size gate (PDF gets a higher cap than images). A spoofed
	// `application/pdf` Content-Type therefore lets a non-PDF up to maxPDFBytes
	// past the metadata gate — this is intentional defense-in-depth: the
	// http.DetectContentType sniff below (R218B-SEC-1) re-validates against
	// magic bytes and rejects spoofed PDFs before we persist anything, and
	// images that lie about being PDFs would still fail the image-prefix path
	// since `declared` would not start with "image/". The size gate is "best
	// effort early reject", not the final authority.
	isPDF := declared == "application/pdf"
	if isPDF && !allowPDF {
		return cli.Attachment{}, fmt.Errorf("PDF attachments must be sent via /api/sessions/upload")
	}

	// Size gates before read: refuse oversize on metadata alone so we don't
	// pull a 50 MB file into memory just to reject it.
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
		// Wrapped os.PathError can surface the temp-file path; keep that for
		// operator logs, return a generic message to the client.
		slog.Debug("upload: open multipart file failed", "err", err)
		return cli.Attachment{}, errors.New("failed to read uploaded file")
	}
	defer f.Close()

	// R247-SEC-11 (#503): magic-byte sniff a 512-byte head BEFORE buffering
	// the full body. The previous flow trusted `declared` to pick the size
	// gate, so a caller asserting Content-Type: application/pdf could force
	// a 32 MB allocation per request even when the body wasn't a PDF — the
	// magic-byte recheck only ran AFTER io.ReadAll(LimitReader(... maxPDFBytes)).
	// Sniffing the head first lets us collapse the buffer cap to maxImageBytes
	// when the declared-PDF body doesn't actually start with %PDF-.
	//
	// We peek 512 bytes (http.DetectContentType's documented maximum) into
	// a fixed-size buffer, sniff against the head, then resume reading the
	// rest of the body via an io.MultiReader so the head is not lost.
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

	// fh.Size comes from the Content-Disposition header (client-controlled).
	// The size gate above rejects oversize uploads based on that header, but
	// a lying client could understate size to bypass the gate. Wrap the
	// reader in a LimitReader as a defence-in-depth byte cap, with a +1
	// margin so we can detect overflow.
	//
	// R247-SEC-11: when the caller declared PDF but the head doesn't carry
	// the %PDF- magic (or is too short to carry it), tighten the buffer cap
	// to maxImageBytes. The downstream PDF branch will still reject the body
	// for the same reason, but the runtime guarantee — peak in-memory bytes
	// per request — now scales with the SNIFFED type, not the declared one.
	headLooksPDF := len(head) >= 5 && bytes.Equal(head[:5], []byte("%PDF-"))
	var sizeLimit int64
	switch {
	case isPDF && headLooksPDF:
		sizeLimit = maxPDFBytes
	case isPDF && !headLooksPDF:
		// Fail fast: a declared-PDF without the magic header cannot be
		// a legitimate PDF, so allocating up to 32 MB to confirm what we
		// already know is wasteful.
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

	// RNEW-SEC-002: defence-in-depth against a PDF-shaped gzip bomb. Our
	// downstream doesn't decompress attachments today, but the two-byte
	// gzip magic (0x1F 0x8B) would not trigger DetectContentType's PDF
	// branch — so this check catches it before anything else can. Still
	// we reject explicitly to stop any future component from unwittingly
	// accepting a compressed container for text/pdf.
	if len(data) >= 2 && data[0] == 0x1F && data[1] == 0x8B {
		return cli.Attachment{}, fmt.Errorf("compressed files are not accepted")
	}
	detected := http.DetectContentType(data)
	if isPDF {
		// PDF magic is "%PDF-" in the first 5 bytes; http.DetectContentType
		// returns "application/pdf" for that signature. A caller claiming
		// PDF but sniffing as anything else is either spoofing or corrupt —
		// reject before we persist bytes we can't trust.
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

	// Image path — gate purely on the byte-sniffed `detected` MIME (R244-SEC-P2-1
	// #886). Trusting `declared` (the client-supplied Content-Type) lets a caller
	// either smuggle a non-image body past the image branch by claiming
	// "image/png", or have a legitimate PNG rejected because the client sent
	// "application/octet-stream". `http.DetectContentType` is the authoritative
	// classifier; allowlist the raster formats Claude accepts so a future
	// sniffer change cannot silently let SVG (text/xml) or any other oddity
	// through. The `declared` value is still used above to pick the size gate
	// (PDF gets a higher cap) — that is intentional defense-in-depth, not the
	// final authority.
	switch detected {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		// ok
	default:
		return cli.Attachment{}, fmt.Errorf("only image/* or application/pdf files are accepted")
	}
	// R232-SEC-7 (#1002): defence-in-depth secondary magic scan. http.DetectContentType
	// only inspects the leading bytes; a JFIF (or any image) header followed by an
	// embedded "%PDF-" body slips past the first sniff and would otherwise be
	// persisted as KindImageInline carrying a PDF payload. Scan the first 4 KB
	// for the PDF magic and reject — the offset window is bounded so the cost
	// is constant on legitimate uploads, while a nested PDF container is
	// always shorter than 4 KB into the file (real JFIF headers are ~20-30
	// bytes; %PDF-` would land well before 4 KB to be processed by any
	// downstream PDF reader).
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
// image by http.DetectContentType — contains a "%PDF-" magic sequence in
// the first 4 KB. R232-SEC-7 (#1002): blocks JFIF+PDF nested-container
// bypass where the JFIF header passes the leading-byte sniff but the body
// is actually a PDF.
func pdfNestedInImage(data []byte) bool {
	const scanWindow = 4 * 1024
	end := len(data)
	if end > scanWindow {
		end = scanWindow
	}
	return bytes.Contains(data[:end], pdfMagicSignature)
}

// pdfMagicSignature is the 5-byte signature every conforming PDF starts
// with. Kept at package scope so pdfNestedInImage's bounded-window scan
// does not re-allocate the literal on each upload.
var pdfMagicSignature = []byte("%PDF-")

// hasPersistableAttachment reports whether any attachment needs to hit
// persistFileRefs. file_ref must land on disk or the Read-tool hint has
// nothing to point at; inline images are persisted on a best-effort basis
// so the dashboard lightbox can load the original instead of the 600 px
// thumbnail. If neither applies (e.g. no attachments at all), the caller
// can skip the workspace resolution + persist round trip entirely.
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
// extension (including the leading dot). Returns "" for anything not in
// the allowlist — matches attachment.sanitizeExt so a MIME that slips past
// here also cannot slip past Persist.
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
// Content-Disposition headers, and dashboard previews. Callers may treat
// the empty input as "no filename"; the sanitised output is also "" in
// that case. The output is bounded to maxClientFilenameRunes runes so
// pathologically long names cannot bloat the prompt or sidecar.
//
// Stripping is deliberately aggressive: the multipart filename is fully
// client-controlled and must never be trusted as a path component. It
// reaches three sinks (the .meta sidecar at write time, Content-Disposition
// for download URLs, and the prepended text Claude receives) and a
// malicious value carrying a path traversal could otherwise be reflected
// if ever reused (cases that would be bugs, but cheap to preempt). We
// strip control bytes, collapse path separators to underscores, and
// truncate to a sane length. We do NOT base this on filepath.Base —
// Windows separators ("\\") on a Linux server wouldn't be stripped by
// filepath.Base.
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
			// drop C1 controls and bidi-override runes so they do not
			// reach the .meta sidecar, Content-Disposition header, or
			// the text hint Claude receives.
		case r == '/' || r == '\\':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	// Byte short-circuit: a string ≤ maxClientFilenameRunes bytes cannot
	// exceed maxClientFilenameRunes runes (one byte per ASCII rune is the
	// best case; multi-byte UTF-8 only increases byte count).
	if len(out) > maxClientFilenameRunes && utf8.RuneCountInString(out) > maxClientFilenameRunes {
		runes := []rune(out)
		out = string(runes[:maxClientFilenameRunes])
	}
	return out
}

// maxClientFilenameRunes caps sanitizeClientFilename output length. The
// value reaches three sinks (the .meta sidecar, the prepended text Claude
// receives, and dashboard UI previews); a 4 KB filename would bloat the
// prompt and the sidecar without any legitimate use case. 120 runes is
// plenty for real filenames; longer values are almost certainly adversarial.
// R222-CR-6.
const maxClientFilenameRunes = 120
