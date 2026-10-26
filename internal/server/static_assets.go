// 嵌入式 dashboard 静态资源：embed.FS 变量、一次性读取+ETag/gzip 预计算、
// serveStaticWithETag 304 fast-path。
package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// The whole static/ tree, registered into staticAssets by fs.WalkDir at init.
// Third-party files under static/vendor are byte-identical to their npm dist
// (mermaid's bundle gzipped) and kept under a versioned directory, so their
// responses can be cached as immutable. KaTeX ships only its woff2 fonts: every
// browser the dashboard supports takes the first src of each @font-face.
//
//go:embed static
var staticFS embed.FS

// staticAsset is the once-read, immutable view of an embedded dashboard asset:
// its bytes and precomputed strong-form ETag. embed.FS.ReadFile copies the
// whole file on every call, so each asset is read+hashed exactly once at init
// and handlers share the read-only slice (#1771).
type staticAsset struct {
	// bytes is nil for a gzip-only asset (a vendor file embedded as .gz): only
	// gz stays resident, and the rare client that refuses gzip gets it
	// decompressed per request (#3662).
	bytes []byte
	etag  string
	// gz is the gzip.BestCompression form, precomputed once at init for
	// compressible assets (content is immutable, so level 9 is paid once and
	// beats the middleware's level 1 by ~15%). nil when not precompressed (#1769).
	gz []byte
}

// precompressGzip returns the gzip.BestCompression form of b, or nil if it did
// not actually shrink. The result is copied out of the scratch buffer so its
// (possibly larger) backing array is not pinned.
func precompressGzip(b []byte) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(b); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	if buf.Len() >= len(b) {
		return nil
	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out
}

// staticAssets maps the asset key (basename used by handlers and the 304
// fast-path) to its cached bytes + ETag. Populated once at init; the
// dashboard.html entry is the rendered page (static_versioning.go), so its
// ETag and gzip form are those of the bytes actually served.
// servedAssetVersion is the nz-asset-version that page carries.
var staticAssets, servedAssetVersion = func() (map[string]staticAsset, string) {
	out := map[string]staticAsset{}
	err := fs.WalkDir(staticFS, "static", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		key := strings.TrimPrefix(name, "static/")
		if strings.HasPrefix(key, "vendor/") {
			return addVendorAsset(out, name, key)
		}
		switch {
		case key == "manifest.json", key == "sw.js":
			return addStaticAsset(out, name, key, false)
		case key == "dashboard.html", key == "favicon.svg",
			path.Ext(key) == ".js" && !strings.Contains(key, "/"),
			path.Ext(key) == ".css" && path.Dir(key) == "css":
			return addStaticAsset(out, name, key, true)
		}
		return nil // package.json and anything else under static/ is not served
	})
	if err != nil {
		panic("read static/: " + err.Error())
	}
	raw, ok := out["dashboard.html"]
	if !ok {
		return out, ""
	}
	version := dashboardAssetVersion(out)
	page, err := renderDashboardHTML(raw.bytes, out)
	if err != nil {
		panic("render dashboard.html: " + err.Error())
	}
	out["dashboard.html"] = newStaticAsset(page, true)
	return out, version
}()

func addStaticAsset(out map[string]staticAsset, name, key string, compress bool) error {
	b, err := staticFS.ReadFile(name)
	if err != nil {
		return err
	}
	out[key] = newStaticAsset(b, compress)
	return nil
}

// addVendorAsset registers a servable file under static/vendor by its path
// below static/, the key its /static/vendor/ URL maps to. Fonts are already
// compressed, so only scripts and stylesheets are gzipped. A file embedded as
// <name>.gz (gzip -9n of the release file) is served as <name>, its embedded
// bytes as the gzip form, so a multi-megabyte bundle costs no compression at
// init and no decompressed copy in memory.
func addVendorAsset(out map[string]staticAsset, name, key string) error {
	key, gzipped := strings.CutSuffix(key, ".gz")
	if staticTypes[path.Ext(key)] == "" {
		return nil
	}
	b, err := staticFS.ReadFile(name)
	if err != nil {
		return err
	}
	if gzipped {
		out[key] = newGzipOnlyAsset(b)
	} else {
		out[key] = newStaticAsset(b, path.Ext(key) != ".woff2")
	}
	return nil
}

// newStaticAsset wraps b with its strong ETag (sha256, first 16 bytes, hex)
// and, when compress is set, its precompressed gzip form.
func newStaticAsset(b []byte, compress bool) staticAsset {
	sum := sha256.Sum256(b)
	a := staticAsset{bytes: b, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
	if compress {
		a.gz = precompressGzip(b)
	}
	return a
}

// newGzipOnlyAsset wraps the gzip form gz of an asset that keeps no
// decompressed copy. Its ETag hashes gz: the file is gzip -9n output, so its
// bytes change exactly when the content does, and init never decompresses it.
func newGzipOnlyAsset(gz []byte) staticAsset {
	sum := sha256.Sum256(gz)
	return staticAsset{gz: gz, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
}

// hasStaticAsset reports whether key is in the embedded asset table. Use it,
// not staticAssetBytes(key) != nil, to tell a miss: a gzip-only asset has no
// bytes.
func hasStaticAsset(key string) bool {
	_, ok := staticAssets[key]
	return ok
}

// staticAssetBytes returns the cached, read-only bytes for an embedded asset.
// Callers MUST NOT mutate the returned slice — it is shared across all
// requests. Returns nil when the key is unknown (asset failed to embed) and for
// a gzip-only asset, which writeStaticAssetBody serves.
func staticAssetBytes(key string) []byte {
	return staticAssets[key].bytes
}

// writeStaticAssetBody writes the asset body, preferring the precomputed gzip
// form when it exists and the client accepts gzip. Setting Content-Encoding
// here makes gzipMiddleware.decide() leave the bytes untouched. Caller must
// have set Content-Type / cache headers and handled the 304 case first.
func writeStaticAssetBody(w http.ResponseWriter, r *http.Request, key string) {
	a := staticAssets[key]
	if a.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h := w.Header()
		h.Set("Content-Encoding", "gzip")
		// Vary + drop Content-Length: mirrors gzipMiddleware.decide().
		h.Add("Vary", "Accept-Encoding")
		h.Del("Content-Length")
		if _, err := w.Write(a.gz); err != nil {
			slog.Debug("static asset gz write", "key", key, "err", err)
		}
		return
	}
	if a.bytes == nil && a.gz != nil {
		// Gzip-only: stream the decompression rather than keep it resident.
		// The headers are already out, so an error can only be logged.
		zr, err := gzip.NewReader(bytes.NewReader(a.gz))
		if err == nil {
			_, err = io.Copy(w, zr)
		}
		if err != nil {
			slog.Debug("static asset gunzip write", "key", key, "err", err)
		}
		return
	}
	if _, err := w.Write(a.bytes); err != nil {
		slog.Debug("static asset write", "key", key, "err", err)
	}
}

// serveStaticWithETag attaches the asset's precomputed ETag and, on an
// If-None-Match hit, writes 304 and returns true so the caller skips the body.
// Call it BEFORE touching the body bytes; security headers (CSP/COOP/etc.)
// must already be set by the caller.
func serveStaticWithETag(w http.ResponseWriter, r *http.Request, assetKey string) bool {
	tag := staticAssets[assetKey].etag
	if tag == "" {
		return false
	}
	w.Header().Set("ETag", tag)
	if match := r.Header.Get("If-None-Match"); match != "" {
		// Substring check instead of full RFC 7232 list parsing: the tag is
		// unique enough that a substring hit is a real match.
		if match == "*" || strings.Contains(match, tag) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}
