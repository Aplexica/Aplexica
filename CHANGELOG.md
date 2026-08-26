# Changelog

This changelog starts with the first public release train. Pre-public
development notes are intentionally excluded because they contained private
operational details rather than a durable user-facing release history.

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
