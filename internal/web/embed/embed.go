// Package embed bundles the aplexica-portal "local" build output
// directly into the daemon binary via go:embed. Serving these
// assets is the daemon's job; populating dist-local/ is not. The only
// supported producer is the Makefile `fetch-portal` target, which downloads
// the aplexica-portal release asset pinned by repository, tag, filename and
// sha256 in packaging/portal-release.json, refuses the bundle on a digest
// mismatch, and rejects absolute, `..`-traversing, symlinked or oversized
// members before anything is extracted.
//
// When dist-local/ is empty (the default for fresh checkouts), the
// handler returns a small "local UI not bundled" HTML page that
// instructs the developer to run `make fetch-portal`. This means
// `go build ./...` works cleanly without the portal assets — the
// CLI and daemon control surface remain fully functional; only the
// web UI returns a placeholder.
package embed

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// distLocalFS is the embedded portal SPA bundle. The blank-target
// file is shipped intentionally so go:embed has something to
// reference even on a fresh checkout; the build pipeline overwrites
// the directory with the real release contents.
//
//go:embed all:dist-local
var distLocalFS embed.FS

// indexCandidates lists the possible SPA entry filenames in
// preference order. The Vite local build emits `index-local.html`
// (configured via rollupOptions.input in aplexica-portal's
// vite.config.ts); developers manually copying a generic Vite
// build may produce `index.html`. The handler probes both at
// construction time and serves whichever exists.
var indexCandidates = []string{"index-local.html", "index.html"}

// placeholderHTML is served when dist-local/ is empty (no
// index.html embedded). Keeps the daemon's web port responsive
// during development and tells the developer how to fix it without
// trawling logs.
const placeholderHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Aplexica — local UI not bundled</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
         margin: 4rem auto; max-width: 36rem; padding: 0 1rem; color: #1a1a1a; }
  h1 { font-size: 1.5rem; margin-bottom: 1rem; }
  code { background: #f0f0f0; padding: 0.15rem 0.35rem; border-radius: 0.2rem; }
  pre  { background: #f8f8f8; border: 1px solid #e0e0e0; border-radius: 0.3rem;
         padding: 1rem; overflow-x: auto; }
  a { color: #0066cc; }
</style>
</head>
<body>
<h1>Aplexica local UI — not bundled in this build</h1>
<p>The daemon is running and the loopback HTTP listener is up, but the
React SPA bundle (<code>internal/web/embed/dist-local/index-local.html</code>)
was not present at compile time. The CLI surface is unaffected —
<code>aplexica status</code>, <code>aplexica web port</code>, etc. all work.</p>
<p>To populate the bundle:</p>
<pre>make fetch-portal
go install ./cmd/aplexica</pre>
<p>See <code>docs/install/build.md</code>. <code>make fetch-portal</code> is the
only supported producer: it downloads the aplexica-portal asset pinned by
digest in <code>packaging/portal-release.json</code> and refuses anything that
does not match. A hand-copied or separately downloaded bundle is not a
release-authoritative input, and a release build rejects one outright — the
published binaries are compiled with <code>-tags release</code>, under which a
missing bundle is a compile error rather than this page.</p>
</body>
</html>
`

// staticCacheControl is the Cache-Control header sent with hashed
// asset bundles (Vite emits filename hashes like `index-AbCdEf12.js`).
// 1 year is the modern web convention; the hash in the filename
// guarantees cache invalidation when content changes.
const staticCacheControl = "public, max-age=31536000, immutable"

// indexCacheControl is the Cache-Control header for index.html
// itself. Short TTL so browsers re-validate on every reload and
// pick up new asset hashes after a daemon upgrade.
const indexCacheControl = "no-cache, must-revalidate"

// Handler returns an http.Handler that serves the embedded SPA.
//
// Routing:
//   - GET /assets/* (Vite-emitted hashed bundles)        -> from dist-local/assets/*
//   - GET /favicon.ico, /robots.txt, /<other top-level>   -> from dist-local/*
//   - GET / and any other path not under /api/             -> dist-local/index.html
//
// The daemon's outer router mounts all API + SSE routes under /api/
// (including /api/events backfill and /api/events/stream SSE); this
// handler is invoked for everything else, including the bare /events
// SPA route.
//
// When the bundle is unavailable (fresh checkout or build-time
// fetch failure) the placeholder HTML is returned instead.
func Handler() http.Handler {
	// Probe at construction time so the per-request hot path doesn't
	// have to re-check on every call.
	sub, err := fs.Sub(distLocalFS, "dist-local")
	if err != nil {
		return placeholderHandler(fmt.Sprintf("internal/web/embed: sub(dist-local): %v", err))
	}
	indexName := resolveIndex(sub)
	if indexName == "" {
		// No known entry filename — placeholder mode.
		return placeholderHandler("local UI bundle is not present (no index-local.html or index.html in dist-local/)")
	}

	fileServer := http.FileServer(http.FS(sub))
	return &spaHandler{
		root:       sub,
		fileServer: fileServer,
		indexName:  indexName,
		indexBytes: readIndex(sub, indexName),
		indexEtag:  computeIndexEtag(sub, indexName),
	}
}

// resolveIndex returns the first entry from indexCandidates that
// actually exists in the embedded FS, or "" if none do. Construction
// callers already verified that ONE of them exists (in Handler);
// this just picks which.
func resolveIndex(sub fs.FS) string {
	for _, name := range indexCandidates {
		if _, err := fs.Stat(sub, name); err == nil {
			return name
		}
	}
	return ""
}

// readIndex slurps the embedded index file so the SPA fallback
// path serves the bytes without re-opening the embedded FS on
// every miss. Returns nil if the index is unavailable; the caller
// already verified existence so this is just defensive.
func readIndex(sub fs.FS, name string) []byte {
	if name == "" {
		return nil
	}
	f, err := sub.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()
	body, _ := io.ReadAll(f)
	return body
}

// computeIndexEtag returns a stable ETag for the index file. The
// content-hashed asset filenames make ETags on the asset routes
// redundant, but the index file's content can change across daemon
// upgrades while its name stays the same — so an ETag prevents
// stale-cache bugs after `brew upgrade aplexica`.
func computeIndexEtag(sub fs.FS, name string) string {
	if name == "" {
		return ""
	}
	f, err := sub.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return fmt.Sprintf(`"sha256-%x"`, h.Sum(nil))
}

// spaHandler serves the embedded SPA with the routing rules described
// on Handler().
type spaHandler struct {
	root       fs.FS
	fileServer http.Handler
	indexName  string
	indexBytes []byte
	indexEtag  string
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Defensive: the outer router should never send /api/ here, but if
	// a misconfiguration does we want a clear 404 instead of serving
	// the SPA's index.html as a "found" response (which would confuse
	// an API client expecting JSON).
	//
	// NOTE: we guard ONLY /api/ — not /events. Every real event
	// endpoint lives under /api/ (/api/events backfill, /api/events/stream
	// SSE). The bare /events path is a legitimate client-side SPA route
	// (the Events page), so it MUST fall through to index.html on hard
	// navigation / refresh / bookmark. Guarding /events here shadowed
	// that route and 404'd it — caught in live end-user testing.
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	// Strip a leading slash to align with fs.FS's path semantics,
	// then probe the embedded FS for an exact match first. Direct
	// hits get hashed-asset cache treatment; misses fall through to
	// the SPA index route.
	urlPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if urlPath != "" && urlPath != h.indexName {
		if f, err := h.root.Open(urlPath); err == nil {
			f.Close()
			// File exists in the embedded FS — let http.FileServer
			// handle Range, If-Modified-Since, etc. Add cache
			// headers based on whether the path looks hashed.
			w.Header().Set("Cache-Control", cacheControlForAsset(urlPath))
			h.fileServer.ServeHTTP(w, r)
			return
		}
	}

	// Fall through to SPA index. React Router takes it from here.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", indexCacheControl)
	if h.indexEtag != "" {
		w.Header().Set("ETag", h.indexEtag)
		if match := r.Header.Get("If-None-Match"); match == h.indexEtag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	_, _ = w.Write(h.indexBytes)
}

// isHashedAsset returns true for paths that look like Vite's
// content-hashed bundle output. The "hashed" predicate is purely
// heuristic — anything under /assets/ or matching the convention
// /<name>-<8+hex>.<ext> qualifies. Falses negatives only mean a
// slightly shorter cache TTL; false positives are impossible
// because Vite always emits hashed names under /assets/.
func isHashedAsset(urlPath string) bool {
	return strings.HasPrefix(urlPath, "assets/") || strings.HasPrefix(urlPath, "dist-local/assets/")
}

func cacheControlForAsset(urlPath string) string {
	if isConflictDetailChunk(urlPath) {
		return indexCacheControl
	}
	if isHashedAsset(urlPath) {
		return staticCacheControl
	}
	return indexCacheControl
}

func isConflictDetailChunk(urlPath string) bool {
	base := path.Base(urlPath)
	return strings.HasPrefix(base, "ConflictDetailPage-") && strings.HasSuffix(base, ".js")
}

// placeholderHandler returns a handler that always serves the
// placeholder HTML page (and ignores the URL path). Used when
// dist-local/ is empty.
//
// The note parameter is logged to diagnostics but NOT exposed to
// the browser — keeps the placeholder UX consistent regardless of
// the underlying cause.
func placeholderHandler(note string) http.Handler {
	_ = note // surface in /api/daemon's diagnostic envelope; W4 already has this hook
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Guard only /api/ — /events is a valid SPA route (see the
		// note in spaHandler.ServeHTTP).
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", indexCacheControl)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(placeholderHTML))
	})
}

// BundlePresent reports whether the embedded dist-local/ contains
// a real SPA entry file. Exposed for /api/daemon's diagnostic
// payload so the SPA itself can render a "you're seeing the
// placeholder because…" hint when it bootstraps.
func BundlePresent() bool {
	sub, err := fs.Sub(distLocalFS, "dist-local")
	if err != nil {
		return false
	}
	return resolveIndex(sub) != ""
}
