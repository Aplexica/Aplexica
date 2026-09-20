# Changelog

This changelog starts with the first public release train. Pre-public
development notes are intentionally excluded because they contained private
operational details rather than a durable user-facing release history.

## [1.0.77] - 2026-09-20

### Fixed

- **`aplexica update` now points a Homebrew install at the tap.** The updater
  still carried its pre-tap status: on a Homebrew-owned executable it reported
  that "the Aplexica Homebrew tap has not been advanced yet" and deliberately
  withheld the upgrade command. The tap has carried every release since
  v1.0.74 and is on 1.0.76 today, so the only thing that message told a
  Homebrew user was false, and the one useful line was missing. Those installs
  now get the real status and the `brew` upgrade command. The apt and WinGet
  channels stay withheld, because neither has a published repository to
  upgrade from.
- **`aplexica daemon restart --help` describes what restart does now.** The
  help text still described the self-exec path that v1.0.76 replaced: it
  promised the command re-runs `daemon start` with the caller's flags, and
  said nothing about service managers or about waiting for the daemon to come
  back. It now covers the managed path (`systemctl --user restart` on Linux,
  `launchctl kickstart -k` on macOS), the unmanaged fallback including
  Windows, that flags do not reach a managed daemon, and that restart fails
  rather than reporting a pid if the daemon does not answer in time.

### Changed

- `docs/install/update.md` describes the channels that exist today. It had
  told readers the Homebrew tap was not usable, that Debian users should wait
  for a signed `.deb` that every release since has shipped, and to build with
  a Go version `go.mod` has moved past.

## [1.0.76] - 2026-09-17

### Fixed

- **A blocked adapter is now reported.** When the startup safety snapshot
  fails, the affected agent is gated entirely: it neither imports nor
  receives. Until now `aplexica status` said nothing about it — the daemon
  read as running, the agent read as installed, and its artifact count simply
  stayed at zero, with the only evidence an `ERROR` line in the daemon log.
  Status now names each blocked adapter, its reason, and the command that
  clears it. Two causes are fixed together: blocked agents were absent from
  the adapter-state map entirely, because that map was built only from agents
  that had already been touched and a gated agent never is; and the block had
  no rendering path in the default output. Blocks are also reported through a
  lock-free surface, so a busy orchestrator can no longer swallow them.
  Resolves #16.
- **`aplexica daemon restart` now restarts the service through its manager.**
  It previously stopped the daemon over the control socket and self-exec'd a
  detached replacement. Under systemd that left the unit stopped — a clean
  exit is a success, so `Restart=on-failure` never fired — and the
  replacement ran unsupervised inside the caller's session scope, where it
  died with the shell. The command reported a pid while leaving no daemon
  running. Restart now delegates to systemd or launchd when a unit is
  installed, and in every path verifies the daemon actually answers on the
  control socket before exiting successfully. Resolves #17.
- **Release asset uploads are retried.** The publish step uploaded roughly
  182 MB across thirteen requests with no retry, so a single transient 5xx
  from GitHub destroyed the entire release — and because the release object
  is created before its assets, it destroyed it in public. Three consecutive
  v1.0.76 runs died this way (504, 500, 504), each on the first asset, while
  GitHub reported every system operational. Each upload now gets up to five
  attempts with increasing backoff. The reviewed publication allow-list is
  unchanged: same URL, method, headers and body, and still no release
  creation, edit or deletion beyond the single create. The byte-pinned
  expected step and its program digest in
  `packaging/scripts/test-installer-security.sh` were updated to match.
  Partially addresses #18; the create-before-upload ordering that makes such
  a failure public is still open there.

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

### Known issues

- **Linux, Ubuntu-family distributions:** the default `0002` umask leaves
  `~/.claude` and `~/.codex` at mode `0775`, which the startup safety backup
  rejects. The agent is then blocked and nothing is imported, and `aplexica
  status` currently reports none of it — the daemon looks healthy and the
  artifact counts stay at zero. Work around it with `chmod 700 ~/.claude
  ~/.codex`, then `systemctl --user restart aplexicad`. Tracked in #16, with
  the missing status surface as the primary fix.
- **Linux:** `aplexica daemon restart` does not cooperate with the
  `systemd --user` unit that `setup --install` registers. It reports a pid but
  leaves no daemon running. Use `systemctl --user restart aplexicad`. Tracked
  in #17.

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
