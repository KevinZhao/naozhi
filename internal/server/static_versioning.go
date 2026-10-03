package server

// static_versioning.go — dashboard.html as served: every /static/ URL names
// its asset's content hash, so the browser may keep the file for good.
//
// The page links its stylesheets and entry modules directly; those URLs are
// rewritten in place. The rest of the module graph is reached through
// `import './x.js'`, which the generated import map redirects to the hashed
// URL, so a release re-downloads only the files whose bytes changed and the JS
// itself is never rewritten. modulepreload links fetch the whole graph in one
// wave instead of one round trip per import level. The page itself stays
// no-cache, so the next load after an upgrade picks up the new hashes.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// staticURLAttrRe matches a src/href attribute naming a /static/ asset.
var staticURLAttrRe = regexp.MustCompile(`((?:src|href)=")/static/([^"?#]+)"`)

// assetURLVersion is the v= value naming an asset's current bytes: the first
// 16 hex characters of its ETag, or "" for a malformed ETag.
func assetURLVersion(etag string) string {
	tag := strings.Trim(etag, `"`)
	if len(tag) < 16 {
		return ""
	}
	return tag[:16]
}

func versionedStaticURL(key string, a staticAsset) string {
	return "/static/" + key + "?v=" + assetURLVersion(a.etag)
}

// dashboardModuleKeys returns the asset keys of every dashboard JS module,
// sorted: each .js asset except the service worker, which is served at /sw.js.
func dashboardModuleKeys(assets map[string]staticAsset) []string {
	var out []string
	for k := range assets {
		if strings.HasSuffix(k, ".js") && k != "sw.js" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// dashboardAssetVersion hashes every asset's key and ETag, so it changes
// exactly when some served byte does. The page carries it as the
// nz-asset-version meta for the client to compare against the server's.
func dashboardAssetVersion(assets map[string]staticAsset) string {
	keys := make([]string, 0, len(assets))
	for k := range assets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s %s\n", k, assets[k].etag)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// renderDashboardHTML returns raw with every /static/ URL versioned and, just
// before </head>, the nz-asset-version meta, the import map and one
// modulepreload link per module. assets holds the raw page under its own key.
// An attribute naming an asset not in assets is an error: it would 404.
func renderDashboardHTML(raw []byte, assets map[string]staticAsset) ([]byte, error) {
	var missing []string
	page := staticURLAttrRe.ReplaceAllFunc(raw, func(m []byte) []byte {
		sub := staticURLAttrRe.FindSubmatch(m)
		key := string(sub[2])
		a, ok := assets[key]
		if !ok {
			missing = append(missing, key)
			return m
		}
		return []byte(string(sub[1]) + versionedStaticURL(key, a) + `"`)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("links unembedded assets %v", missing)
	}
	if bytes.Count(page, []byte("</head>")) != 1 {
		return nil, fmt.Errorf("want exactly one </head>")
	}

	modules := dashboardModuleKeys(assets)
	imports := make(map[string]string, len(modules))
	var head strings.Builder
	fmt.Fprintf(&head, "<meta name=\"nz-asset-version\" content=\"%s\">\n", dashboardAssetVersion(assets))
	for _, k := range modules {
		imports["/static/"+k] = versionedStaticURL(k, assets[k])
	}
	importMap, err := json.Marshal(map[string]map[string]string{"imports": imports})
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(&head, "<script type=\"importmap\">%s</script>\n", importMap)
	for _, k := range modules {
		fmt.Fprintf(&head, "<link rel=\"modulepreload\" href=\"%s\">\n", versionedStaticURL(k, assets[k]))
	}
	at := bytes.Index(page, []byte("</head>"))
	out := make([]byte, 0, len(page)+head.Len())
	out = append(out, page[:at]...)
	out = append(out, head.String()...)
	return append(out, page[at:]...), nil
}

// staticCacheControl lets the browser keep an asset for a year only when the
// request names the asset's current hash. An unversioned URL, or a stale v= (a
// page rendered by the previous build during a restart), revalidates, so no
// URL is ever pinned to bytes other than the ones its hash names. private: the
// assets are auth-gated, so a shared cache must not serve them.
func staticCacheControl(r *http.Request, key string) string {
	if v := r.URL.Query().Get("v"); v != "" && v == assetURLVersion(staticAssets[key].etag) {
		return "private, max-age=31536000, immutable"
	}
	return "no-cache, must-revalidate"
}
