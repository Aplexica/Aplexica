# Aplexica top-level Makefile — convenience layer over `go build`.
# Plain `go build ./...` and the existing CI workflows continue to work
# without this; the Makefile only saves typing.

GO     ?= go
BIN    ?= bin
TRAYTAG ?= tray

# Version stamped into the binary. Derived from the nearest git tag so local /
# ad-hoc builds report a clean vX.Y.Z — never `git describe`'s `-<N>-g<hash>`
# suffix (which appears when HEAD is not exactly on a tag). Authoritative
# releases are produced by .github/workflows/release.yml on a vX.Y.Z tag push,
# which passes its own ldflags via .goreleaser.yaml; this variable only affects
# local and ad-hoc builds. Overridable from the environment / command line —
# CI test binaries can leave it empty to use the in-source baseline in
# internal/version.
VERSION_PKG := github.com/aplexica/aplexica/internal/version
GIT_VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null)
GIT_COMMIT  := $(shell git rev-parse HEAD 2>/dev/null)
GIT_DATE    := $(shell git show -s --format=%cI HEAD 2>/dev/null)
ifeq ($(strip $(GIT_VERSION)),)
LDFLAGS ?=
else
LDFLAGS ?= -X $(VERSION_PKG).Version=$(GIT_VERSION) -X $(VERSION_PKG).GitCommit=$(GIT_COMMIT) -X $(VERSION_PKG).BuildDate=$(GIT_DATE)
endif

ifeq ($(GOOS),windows)
TRAY_WINDOWS_LDFLAGS ?= -H=windowsgui
else ifeq ($(OS),Windows_NT)
TRAY_WINDOWS_LDFLAGS ?= -H=windowsgui
else
TRAY_WINDOWS_LDFLAGS ?=
endif
ifneq ($(strip $(GIT_VERSION)),)
TRAY_VERSION_LDFLAGS ?= -X main.trayVersion=$(GIT_VERSION)
endif
TRAY_LDFLAGS ?= $(strip $(LDFLAGS) $(TRAY_VERSION_LDFLAGS) $(TRAY_WINDOWS_LDFLAGS))

.PHONY: all build status-helper tray test test-race lint magiclint magiclint-init clean fetch-portal fetch-portal-git verify-portal

all: build status-helper

# Main daemon + CLI binary. Plain build, no special tags.
build:
	mkdir -p $(BIN)
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/aplexica ./cmd/aplexica

# Same CLI surface, installed under a distinct executable name so the tray's
# status watcher is clearly labeled in process monitors.
status-helper:
	mkdir -p $(BIN)
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/aplexica-status ./cmd/aplexica

# Cross-platform tray indicator. Requires -tags tray; pulls in the
# system-tray UI library (added in v0.36.0). Build is gated so plain
# `go build ./...` and the existing CI matrix do NOT need any GUI dev
# libraries.
tray:
	mkdir -p $(BIN)
	$(GO) build -tags $(TRAYTAG) -ldflags '$(TRAY_LDFLAGS)' -o $(BIN)/aplexicatray ./cmd/aplexicatray

test:
	$(GO) test ./... -timeout 240s

test-race:
	$(GO) test -race ./... -timeout 240s

# FR-10.6 magic-number lint. CI runs this; locally, run it before
# pushing a change that adds a new tunable. New violations not in
# .magiclint-allow will fail the build.
lint: magiclint

magiclint:
	$(GO) run ./tools/magiclint

# Regenerate .magiclint-allow from the current source tree. Only use
# this when you've reduced (or intentionally grown) the allowlist —
# the file is a deliberate budget, not a generated artifact.
magiclint-init:
	$(GO) run ./tools/magiclint --init-allowlist

clean:
	rm -rf $(BIN)

# Stage the local web UI bundle into internal/web/embed/dist-local/ so
# go:embed compiles the SPA into the daemon binary. The exact aplexica-portal
# release — repository, tag, asset filename and SHA-256 — is pinned in
# packaging/portal-release.json, so bumping the portal is a one-line reviewable
# diff in this repo. The digest is the binding, not the download: a re-uploaded
# release asset cannot substitute a different bundle without the pin changing
# in a reviewed commit.
#
# This target is the ONLY implementation of the fetch — the release workflow
# invokes it too, so CI and a developer laptop cannot drift apart on what "the
# portal bundle" means.
#
# The local-only Portal source and its pinned release asset are public. Fetching
# anonymously is intentional: the OSS release must not depend on private
# repository credentials or expose a cross-repository token to code in the
# tagged tree.
#
# Without this target the daemon still builds — the placeholder HTML in
# internal/web/embed/embed.go renders a "local UI not bundled" page on the
# loopback listener, and the CLI surface is unaffected. Release builds are
# stricter by design: internal/web/embed/embed_release.go turns a missing
# bundle into a compile error under `-tags release`.
PORTAL_PIN  ?= packaging/portal-release.json
PORTAL_DIST ?= internal/web/embed/dist-local

# Caps applied to the archive BEFORE a single byte is extracted. They sit far
# above a real portal bundle (~3.7 MB expanded across 86 entries) and exist
# only so a malformed or hostile tarball cannot fill the disk on the way in.
# They are NOT decompression-bomb protection, and reading them that way is the
# trap: the two listing passes below already stream the whole decompressed
# archive through tar before the caps are evaluated, so a bomb costs its full
# CPU and wall-clock time either way. What bounds THAT is the digest pin —
# nothing is unpacked, listed or read until the bytes match packaging/portal-release.json.
PORTAL_MAX_ENTRIES     ?= 100000
PORTAL_MAX_ENTRY_BYTES ?= 268435456
PORTAL_MAX_TOTAL_BYTES ?= 4294967296

# The recipe is one continued shell line, so it carries no inline `#` comments
# (a `#` would swallow the rest of the joined line). Four notes that would
# otherwise live next to the code:
#
#   * The safety walk runs entirely off the archive's index, before extraction.
#     It rejects absolute and ..-traversing member paths, every member that is
#     not a regular file or a directory (a symlink or hard link is how a
#     tarball writes outside its extraction root; devices, FIFOs and sockets
#     have no business in a web bundle), and anything past the caps above.
#   * `tar -tv` prints the size in a different column depending on the tar, so
#     the awk below branches on the FORMAT rather than scanning for a date.
#     GNU packs owner and group into one slash-joined field
#     ("mode user/group size YYYY-MM-DD HH:MM name"); BSD never does
#     ("mode links user group size Mon DD HH:MM name"). A slash in field 2
#     therefore means GNU and the size is field 3; a bare integer link count in
#     field 2 means BSD and the size is field 5. Scanning for the timestamp
#     instead used to fail OPEN: a stored owner or group of exactly three
#     letters starting with a capital (`Bob`, `Adm`, `Www`) matched the
#     month-abbreviation pattern, so the parser read the link count — always 0
#     — as the size, and BOTH caps silently passed on any archive.
#   * A line matching neither shape, or whose size field is not all digits, is
#     a listing we do not understand, and we refuse to extract an archive we
#     cannot account for.
#   * awk's `exit` runs the END block on its way out, so a per-entry rejection
#     would otherwise print its own message AND the total-cap message computed
#     from a partial total. The `bailed` flag keeps the first, real diagnostic
#     the only one; the exit status set by `exit 1` survives END untouched.
fetch-portal:
	@set -eu; \
	  pin='$(PORTAL_PIN)'; dist='$(PORTAL_DIST)'; \
	  field() { sed -n 's/.*"'"$$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$$pin"; }; \
	  digest_of() { if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$$1"; else sha256sum "$$1"; fi; }; \
	  repo=$$(field repository); tag=$$(field tag); asset=$$(field asset); want=$$(field sha256); \
	  [ -n "$$repo" ] && [ -n "$$tag" ] && [ -n "$$asset" ] && [ -n "$$want" ] \
	    || { printf '%s: repository, tag, asset and sha256 must all be set\n' "$$pin" >&2; exit 1; }; \
	  [ "$$repo" = 'Aplexica/aplexica-portal' ] \
	    || { printf '%s: repository must be Aplexica/aplexica-portal\n' "$$pin" >&2; exit 1; }; \
	  printf '%s\n' "$$tag" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$$' \
	    || { printf '%s: tag is not a canonical release tag\n' "$$pin" >&2; exit 1; }; \
	  [ "$$asset" = "aplexica-portal-$$tag-local.tar.gz" ] \
	    || { printf '%s: asset does not match the pinned tag\n' "$$pin" >&2; exit 1; }; \
	  printf '%s\n' "$$want" | grep -Eq '^[0-9a-f]{64}$$' \
	    || { printf '%s: sha256 is not a lowercase SHA-256\n' "$$pin" >&2; exit 1; }; \
	  tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT INT TERM HUP; \
	  url="https://github.com/$$repo/releases/download/$$tag/$$asset"; \
	  curl --fail --location --silent --show-error --retry 3 --retry-all-errors \
	    --output "$$tmp/$$asset" "$$url" \
	    || { printf 'portal bundle fetch failed: %s\n' "$$url" >&2; exit 1; }; \
	  got=$$(digest_of "$$tmp/$$asset" | awk '{ print $$1 }'); \
	  if [ "$$got" != "$$want" ]; then \
	    printf 'portal bundle digest mismatch for %s\n  pinned:  %s\n  fetched: %s\n' "$$asset" "$$want" "$$got" >&2; \
	    exit 1; \
	  fi; \
	  tar -tzf "$$tmp/$$asset" > "$$tmp/names"; tar -tvzf "$$tmp/$$asset" > "$$tmp/listing"; \
	  if grep -Eq '^/|(^|/)\.\.(/|$$)' "$$tmp/names"; then \
	    printf 'portal bundle rejected: absolute or ..-traversing member path\n' >&2; exit 1; \
	  fi; \
	  if grep -Eq '^[^-d]| -> | link to ' "$$tmp/listing"; then \
	    printf 'portal bundle rejected: contains a member that is not a regular file or a directory\n' >&2; exit 1; \
	  fi; \
	  if [ "$$(wc -l < "$$tmp/names")" -gt $(PORTAL_MAX_ENTRIES) ]; then \
	    printf 'portal bundle rejected: more than %s entries\n' '$(PORTAL_MAX_ENTRIES)' >&2; exit 1; \
	  fi; \
	  awk -v max=$(PORTAL_MAX_ENTRY_BYTES) -v cap=$(PORTAL_MAX_TOTAL_BYTES) ' \
	    { if ($$2 ~ "/") size = $$3; else if (NF >= 9 && $$2 ~ /^[0-9]+$$/) size = $$5; else size = ""; \
	      if (size !~ /^[0-9]+$$/) { print "unparsable tar listing: " $$0 > "/dev/stderr"; bailed = 1; exit 1 }; \
	      size = size + 0; total += size; \
	      if (size > max) { print "entry over the per-entry cap: " $$0 > "/dev/stderr"; bailed = 1; exit 1 } } \
	    END { if (!bailed && total > cap) { printf "expanded bundle is %d bytes, over the cap\n", total > "/dev/stderr"; exit 1 } } \
	  ' "$$tmp/listing" || { printf 'portal bundle rejected: size cap exceeded\n' >&2; exit 1; }; \
	  mkdir -p "$$dist"; \
	  find "$$dist" -mindepth 1 -maxdepth 1 ! -name .gitkeep -exec rm -rf {} +; \
	  tar -xzf "$$tmp/$$asset" -C "$$dist" --strip-components=1; \
	  [ -f "$$dist/index-local.html" ] \
	    || { printf 'portal bundle rejected: %s/index-local.html missing after extract\n' "$$dist" >&2; exit 1; }; \
	  [ -n "$$(find "$$dist/assets" -type f -name '*.js' -print -quit 2>/dev/null)" ] \
	    || { printf 'portal bundle rejected: %s/assets holds no .js bundle\n' "$$dist" >&2; exit 1; }; \
	  printf '%s %s %s %s\n' "$$repo" "$$tag" "$$asset" "$$want" > "$$dist/.portal-pin"; \
	  printf 'staged %s %s (%s) into %s\n' "$$repo" "$$tag" "$$asset" "$$dist"


# Exact-commit git fetch of Aplexica/aplexica-portal. Requires PORTAL_FETCH_TOKEN
# and PORTAL_SOURCE_COMMIT (40 lowercase hex). Does not replace fetch-portal.
PORTAL_SOURCE_COMMIT ?=
PORTAL_GIT_DIR ?= .portal-git
fetch-portal-git:
	@test -n "$$PORTAL_FETCH_TOKEN" || { printf 'PORTAL_FETCH_TOKEN is required\n' >&2; exit 1; }
	@test -n "$(PORTAL_SOURCE_COMMIT)" || { printf 'PORTAL_SOURCE_COMMIT is required\n' >&2; exit 1; }
	./packaging/scripts/fetch-portal-git.sh --repo Aplexica/aplexica-portal --commit "$(PORTAL_SOURCE_COMMIT)" --output-dir "$(PORTAL_GIT_DIR)"

# Is the bundle currently staged in dist-local/ the one the pin names?
#
# `go build -tags release` cannot answer that. It has no dependency on
# packaging/portal-release.json — the go:embed directive only asks whether SOME
# index-local.html and SOME assets/ exist — so a developer whose dist-local was
# staged from an older pin builds a release-tagged binary carrying the wrong UI
# and nothing complains.
#
# CI is structurally immune (fresh checkout, dist-local/* is gitignored, and
# the release job runs fetch-portal before it builds), so this target is
# local-dev hygiene and is deliberately NOT wired into `build`: an unstaged
# dist-local is the correct state for ordinary development, where the
# placeholder page is expected.
#
# The stamp lives inside dist-local so it is wiped and rewritten with the
# bundle it describes — a stamp beside the directory could outlive an `rm -rf`
# of it and then assert something false. It is gitignored along with the rest
# of dist-local/*. It is also embedded (embed.go uses `all:dist-local`, which
# does include dot-files) and therefore reachable at /.portal-pin on the
# loopback UI; that is four fields already published verbatim in
# packaging/portal-release.json, so there is nothing there to leak.
verify-portal:
	@set -eu; \
	  pin='$(PORTAL_PIN)'; dist='$(PORTAL_DIST)'; \
	  field() { sed -n 's/.*"'"$$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$$pin"; }; \
	  repo=$$(field repository); tag=$$(field tag); asset=$$(field asset); want=$$(field sha256); \
	  [ -n "$$repo" ] && [ -n "$$tag" ] && [ -n "$$asset" ] && [ -n "$$want" ] \
	    || { printf '%s: repository, tag, asset and sha256 must all be set\n' "$$pin" >&2; exit 1; }; \
	  stamp="$$dist/.portal-pin"; \
	  [ -f "$$stamp" ] \
	    || { printf 'no portal bundle staged in %s (no .portal-pin stamp)\n' "$$dist" >&2; \
	         printf '  Run "make fetch-portal" before building with -tags release.\n' >&2; exit 1; }; \
	  wanted=$$(printf '%s %s %s %s' "$$repo" "$$tag" "$$asset" "$$want"); \
	  got=$$(cat "$$stamp"); \
	  [ "$$got" = "$$wanted" ] \
	    || { printf 'staged portal bundle does not match %s\n  pinned: %s\n  staged: %s\n' "$$pin" "$$wanted" "$$got" >&2; \
	         printf '  Run "make fetch-portal" to restage.\n' >&2; exit 1; }; \
	  printf 'staged portal bundle matches %s (%s %s)\n' "$$pin" "$$repo" "$$tag"
