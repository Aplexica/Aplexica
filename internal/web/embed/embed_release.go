//go:build release

// This file is compiled ONLY for release builds (.goreleaser.yaml puts
// `release` in the `tags` list of every builds entry). It converts a missing
// local UI bundle from a silent runtime placeholder into a hard COMPILE
// failure, so a published Aplexica release can never ship without the real
// web UI.
//
// dist-local/index-local.html is the Vite "local" entry that only a genuine
// aplexica-portal build/release produces. A fresh checkout has the dev
// fallback instead — a bare placeholder, no index-local.html — which does NOT
// satisfy the directive below, so it fails to build under `-tags release`
// rather than shipping the "local UI not bundled" page to end users.
//
// dist-local/assets is embedded too: a real Vite build always emits hashed
// JS/CSS bundles there, so an empty (or absent) assets directory — the
// signature of a stub/placeholder bundle — also fails the compile.
//
// Pairs with the "Stage the pinned portal bundle" step of
// .github/workflows/release.yml, which runs `make fetch-portal` to stage the
// aplexica-portal release asset pinned by digest in
// packaging/portal-release.json into dist-local/. Defense in depth: if that
// step is ever removed, reordered, or silently fetches nothing, this guard
// still turns the result into a build failure instead of a release.

package embed

import "embed"

//go:embed dist-local/index-local.html
//go:embed dist-local/assets
var requireRealPortalBundle embed.FS
