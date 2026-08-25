# Build from source

Aplexica is a Go project. The repository's `go.mod` requires **Go 1.25.12**;
use that exact version for supported builds and test results.

## Prerequisites

- [Go 1.25.12](https://go.dev/dl/)
- Git
- `make` for the convenience targets (optional)

Confirm the toolchain before building:

```bash
go version
```

## Clone and build

```bash
git clone https://github.com/Aplexica/Aplexica.git
cd Aplexica
mkdir -p bin

go build -o bin/aplexica ./cmd/aplexica
# Not a typo: aplexica-status is the same program under a second name, so the
# tray's status watcher is clearly labeled in process monitors. The release
# build does exactly this too.
go build -o bin/aplexica-status ./cmd/aplexica
go build -tags tray -o bin/aplexicatray ./cmd/aplexicatray
```

On Windows, give the outputs an `.exe` suffix. The equivalent convenience
targets on macOS and Linux are:

```bash
make all
make tray
```

A normal source checkout can build the CLI and daemon without a bundled Portal
SPA. In that case the loopback web endpoint shows a placeholder while the CLI
and daemon remain usable. Use a published package when you need the complete
release bundle. Release-production procedures are intentionally outside this
end-user build guide.

The `release` build tag is the exception: it requires a staged Portal bundle
in `internal/web/embed/dist-local/`, which `make fetch-portal` produces. A
build with `-tags release` and no staged bundle fails to compile on purpose,
so a release can never ship a placeholder in place of the local web UI. The
plain `go build` commands above omit that tag and are unaffected.

`make fetch-portal` anonymously downloads the exact public local-mode Portal
bundle pinned in `packaging/portal-release.json` and verifies its SHA-256
before staging it. The public Portal distribution contains no Cloud-mode code.

## Source authenticated by the release signature

`gh repo clone` and `git clone` give you source authenticated only by TLS and
by GitHub's word for it. Every release also publishes the corresponding source
as a release asset:

```text
aplexica-<VERSION>-source.tar.gz
```

That tarball is listed in the release's `SHA256SUMS`, whose cosign signature is
authorized by the release AWS KMS key, so it is
source you can authenticate with exactly the same command you use for a binary
archive. Follow [Verify a release](verify.md), substituting the source tarball
for the platform archive in the download and digest-check steps. It unpacks
into an `aplexica-<VERSION>/` directory rather than into the current one.

The complete corresponding source for a release consists of this daemon source
archive plus the public local Portal source named by
`packaging/portal-release.json`. The daemon archive is a `git archive` of the
tag and builds like that checkout; `make fetch-portal` obtains the separately
versioned, digest-pinned Portal input for `-tags release`.

## Run the tests

```bash
go test ./... -timeout 240s
go vet ./...
```

On a platform that supports the Go race detector, also run:

```bash
go test -race ./... -timeout 240s
```

Changes to the tray can be checked separately:

```bash
go test -tags tray ./cmd/aplexicatray/...
```

## Install a local build

The following user-scoped example avoids replacing operating-system-managed
binaries:

```bash
mkdir -p "$HOME/.local/bin"
install -m 0755 ./bin/aplexica "$HOME/.local/bin/aplexica"
install -m 0755 ./bin/aplexica-status "$HOME/.local/bin/aplexica-status"
install -m 0755 ./bin/aplexicatray "$HOME/.local/bin/aplexicatray"
```

Add `$HOME/.local/bin` to your `PATH` if it is not already present, open a new
shell, then run:

```bash
aplexica setup --yes --install
aplexica status
```

## Uninstall a local build

First unregister the background services while the executable is still
available:

```bash
aplexica daemon uninstall
aplexica tray uninstall
```

Then remove only the three local executables from the directory where you
installed them. User data under `~/.aplexica/` remains in place. Back up or
archive that directory before deciding whether to discard any data; see the
[general uninstall guidance](_index.md#uninstalling).
