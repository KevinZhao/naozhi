package project

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// sanitizeDownloadName strips control characters and path separators from
// the filename used in Content-Disposition: CR/LF would enable response
// splitting, and C1 controls / bidi overrides (osutil.IsLogInjectionRune)
// survive percent-encoding to confuse intermediaries or make `foo.txt`
// render as `foo.exe` in the UI.
func sanitizeDownloadName(p string) string {
	base := filepath.Base(p)
	var b strings.Builder
	b.Grow(len(base))
	for _, r := range base {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop C0 controls
		case osutil.IsLogInjectionRune(r):
			// drop C1 controls + bidi override / isolate + LS / PS
		case r == '"', r == '\\':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "download"
	}
	return out
}

// contentDisposition builds an RFC 6266 / RFC 5987 Content-Disposition
// value: pure-ASCII names use the plain quoted form for legacy clients,
// non-ASCII names add the `filename*=UTF-8”...` form so strict
// intermediaries do not mangle them.
func contentDisposition(kind, resolved string) string {
	name := sanitizeDownloadName(resolved)
	ascii := true
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return fmt.Sprintf(`%s; filename="%s"`, kind, name)
	}
	// Emit both forms: ASCII fallback (with non-ASCII stripped to '_') for
	// legacy clients + RFC 5987 UTF-8 form for modern browsers.
	asciiFallback := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 0x80 {
			asciiFallback = append(asciiFallback, '_')
		} else {
			asciiFallback = append(asciiFallback, c)
		}
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, asciiFallback, url.PathEscape(name))
}

// sensitiveDownloadNames lists exact filenames that commonly contain
// credentials; compared case-insensitively so ".ENV" doesn't slip through.
var sensitiveDownloadNames = map[string]struct{}{
	".env":             {},
	".env.local":       {},
	".env.dev":         {},
	".env.development": {},
	".env.prod":        {},
	".env.production":  {},
	".env.staging":     {},
	".env.test":        {},
	".netrc":           {},
	".npmrc":           {},
	".pypirc":          {},
	".dockercfg":       {},
	// SSH keys / authorized_keys carry no extension, so the ext list misses them.
	"id_rsa":          {},
	"id_dsa":          {},
	"id_ecdsa":        {},
	"id_ed25519":      {},
	"authorized_keys": {},
	"credentials":     {}, // ~/.aws/credentials, docker credentials helpers, etc.
	// Cloud-native credential filenames whose .json / .yaml extensions are too
	// broad for the extension allowlist; matched by full filename.
	"service-account.json":                 {},
	"serviceaccount.json":                  {},
	"service_account.json":                 {},
	"secrets.yaml":                         {},
	"secrets.yml":                          {},
	"secrets.json":                         {},
	"secret.yaml":                          {},
	"secret.yml":                           {},
	"gcp-key.json":                         {},
	"gcp_key.json":                         {},
	"gcloud-key.json":                      {},
	"firebase-adminsdk.json":               {},
	"application_default_credentials.json": {},
	"kubeconfig":                           {}, // legacy short name, also picked up via path
	// Ops-conventional credential files (Rails database.yml, DSN bundles,
	// api-keys.*); exact matches so "data.yml" / "config.yml" still preview.
	"database.yml":     {},
	"database.yaml":    {},
	"credentials.yml":  {},
	"credentials.yaml": {},
	"credentials.json": {},
	"api-keys.json":    {},
	"api-keys.yml":     {},
	"api-keys.yaml":    {},
	"api_keys.json":    {},
	"api_keys.yml":     {},
	"api_keys.yaml":    {},
	"rds.yml":          {},
	"rds.yaml":         {},
	"pg.yml":           {},
	"pg.yaml":          {},
	"mysql.yml":        {},
	"mysql.yaml":       {},
}

// sensitiveBaseSuffixes lists suffixes that identify backups / archives of
// credential files (".env.bak", ".env.old") so the exact-match table need not
// grow combinatorially.
var sensitiveBaseSuffixes = []string{
	".env.backup",
	".env.bak",
	".env.old",
	".env.orig",
	".env.save",
}

// sensitiveNameSubstrings is a defence-in-depth scan layered on the
// exact-name / extension / suffix rules (#1680): Claude routinely writes
// ad-hoc credential dumps with non-canonical names (`db-password.txt`,
// `aws_credentials.txt`, `api_token.log`). Matched case-insensitively as a
// basename substring. Kept deliberately narrow — no bare "key" token, since
// *.key is handled by sensitiveDownloadExts and "key" would block
// "keyboard.go" / "monkey.png".
var sensitiveNameSubstrings = []string{
	"password",
	"passwd",
	"secret",
	"credential", // matches credential / credentials, hyphen/underscore forms
	"token",
	"apikey",
	"api-key",
	"api_key",
	"private-key",
	"private_key",
	"privatekey",
}

// sensitiveDownloadExts lists extensions that strongly imply key material.
var sensitiveDownloadExts = map[string]struct{}{
	".key": {},
	".pem": {},
	".p12": {},
	".pfx": {},
	".crt": {}, // certs are usually fine, but combined with adjacent .key files
	".p8":  {}, // Apple/AWS/JWT private keys
}

// sensitivePathSegments lists directory names that, anywhere in the path,
// mark the whole subtree as credential-bearing, so `secrets/db.yaml` or
// `.ssh/known_hosts` cannot be exfiltrated on the strength of an innocent
// basename. Matched case-insensitively against every segment by
// isSensitiveDownloadPath; isSensitiveDownloadName keeps the basename contract.
var sensitivePathSegments = map[string]struct{}{
	".ssh":         {},
	".aws":         {},
	".gnupg":       {},
	".gpg":         {},
	".kube":        {},
	".docker":      {},
	".gcloud":      {},
	".azure":       {},
	"secrets":      {},
	"credentials":  {},
	"private-keys": {},
}

// isSensitiveDownloadPath reports whether any segment of relPath looks
// credential-bearing (sensitivePathSegments) or its basename does
// (isSensitiveDownloadName). Both `/` and the OS separator are honoured.
func isSensitiveDownloadPath(relPath string) bool {
	if relPath == "" {
		return false
	}
	// Split on both separators so a Windows-style path cannot bypass the scan.
	norm := strings.ReplaceAll(relPath, "\\", "/")
	for _, seg := range strings.Split(norm, "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		low := strings.ToLower(seg)
		if _, ok := sensitivePathSegments[low]; ok {
			return true
		}
	}
	return isSensitiveDownloadName(filepath.Base(relPath))
}

// isSensitiveDownloadName reports whether base names a well-known
// credential-bearing file by fixed name, dotenv rule, extension or suffix.
func isSensitiveDownloadName(base string) bool {
	low := strings.ToLower(base)
	if _, ok := sensitiveDownloadNames[low]; ok {
		return true
	}
	// One rule for every dotenv variant (.env, .env.local, .env.example, …);
	// templates routinely carry secrets that became real. `.env` must be
	// followed by end-of-string or `.` so `.envoy.yaml` keeps previewing
	// (pinned in TestIsSensitiveDownloadName_OpsConventional).
	if low == ".env" || strings.HasPrefix(low, ".env.") {
		return true
	}
	if ext := filepath.Ext(low); ext != "" {
		if _, ok := sensitiveDownloadExts[ext]; ok {
			return true
		}
	}
	// Suffix scan for ".env.backup" / ".env.bak" style archive names.
	for _, suffix := range sensitiveBaseSuffixes {
		if strings.HasSuffix(low, suffix) {
			return true
		}
	}
	// Defence-in-depth substring scan for ad-hoc credential dumps (#1680).
	for _, sub := range sensitiveNameSubstrings {
		if strings.Contains(low, sub) {
			return true
		}
	}
	return false
}
