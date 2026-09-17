# Changelog

This changelog starts with the first public release train. Pre-public
development notes are intentionally excluded because they contained private
operational details rather than a durable user-facing release history.

## [1.0.75] - 2026-09-17

### Added

- An independent release verifier (`verify-release.yml`) re-checks a published
  release from a clean checkout of the tagged trust policy: it re-derives every
  artifact digest, verifies the KMS signature over `SHA256SUMS` and the SLSA
  provenance statement, and refuses a release whose published bytes disagree
  with the signed manifest. The installer security suite covers the same
  assertions, and [RELEASING.md](docs/RELEASING.md) documents the procedure.

### Changed

- `aplexica doctor` report now includes a `build:` line showing the commit hash,
  build date, and a `(modified)` marker when the binary was built from a dirty
  working tree. For release and ordinary `make` builds the values are stamped by
  ldflags; for plain `go build` they fall back to the `vcs.*` build-settings
  embedded by the Go toolchain.
- `aplexica status` no longer reports writes held by your own routing policy as
  "pending retries". Artifacts bound for an agent you have not enabled are
  queued deliberately, so they materialize the moment that agent is enabled;
  counting them as retries made a correctly configured device look like it had
  a large backlog of failures. They are now named as configuration, with the
  command that releases them. Writes held by a fault still count as retries,
  and an unrecognized suppression reason is still treated as a fault.
- The Windows install path is the versioned `.zip` download rather than WinGet,
  which has no published manifest yet. [windows.md](docs/install/windows.md)
  covers the download, the tray Startup shortcut, and the daemon logon task.
- The Homebrew tap is documented as live, and the README leads with the
  Claude Code and Codex pair, a 60-second try-it, and an explicit list of the
  adapters that do not exist yet.
- The README now says plainly that `aplexica sync enable --all` enables every
  installed agent at once, and suggests closing a running agent or naming
  receivers individually.

## [1.0.74] - 2026-08-25

### Changed

- In-source version baseline matches this tag.
- The release pipeline is redesigned deterministic and hosted-only: every job
  runs on an ephemeral GitHub-hosted runner, nothing moves between jobs except
  bounded, validated, base64-encoded job outputs (the signed checksum manifest,
  both signature bundles, and the provenance statement), and the publish job
  independently rebuilds all ten payloads and requires byte identity with the
  KMS-signed manifest before the one non-draft publication. No Actions
  artifacts, no Actions cache, no draft release at any point.
- The portal bundle is fetched anonymously from its public release and bound
  by the digest pin alone; the staged-source override is removed.
- Daemon pin corrected: Portal v0.1.12 (`aplexica-portal-v0.1.12-local.tar.gz`)
  is bound to digest 5b63fb17, the digest of the published public asset.

## [1.0.73] - 2026-08-23

### Changed

- In-source version baseline matches this tag.
- Publish creates the GitHub release with curl + GITHUB_TOKEN (no `gh` on the fleet runner).
- Daemon pin remains Portal v0.1.12 (`aplexica-portal-v0.1.12-local.tar.gz`, digest 255ea1ff).

## [1.0.72] - 2026-08-23

### Changed

- In-source version baseline matches this tag.
- Release guard reads repository visibility from the GitHub event (no `gh` on the fleet runner).
- Daemon pin remains Portal v0.1.12 (`aplexica-portal-v0.1.12-local.tar.gz`, digest 255ea1ff).

## [1.0.70] - 2026-08-15

### Changed

- Releases now run from an annotated version tag through GoReleaser, isolated
  AWS KMS signing, GitHub publication, and public verification. Each release
  carries a KMS-authorized checksum bundle and a strict ten-subject SLSA v1
  provenance bundle, both verified with the independently distributed
  `aplexica-release.pub` trust anchor.
- Release builds embed the public, digest-pinned local Portal distribution;
  Cloud-mode Portal code is not a daemon build input.
- `aplexica update` is advisory only. It reports release metadata and the
  appropriate channel-specific upgrade guidance; it does not download,
  authenticate, stage, or replace executable files.
- Direct archives, exact `.deb` assets, and source builds are documented as
  available. Homebrew remains paused until the binary-formula bump; WinGet
  remains paused until its first accepted publication.

### Removed

- Removed unsupported legacy release tooling, unpublished direct-installer
  scripts, and their companion helper binaries. The supported direct path is
  a manually downloaded archive authenticated with the public KMS trust anchor.
