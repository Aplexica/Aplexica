# Installing Aplexica

This page reflects the channels available for the public launch. Aplexica
provides three executables:

- `aplexica` — the CLI, daemon, and local web server;
- `aplexica-status` — the status helper used by the tray; and
- `aplexicatray` — the system-tray application.

## Installation channels

| Platform | Channel | Public-launch status | Instructions |
|---|---|---|---|
| Debian / Ubuntu (`amd64`, `arm64`) | `.deb` release asset | Available for published releases. Select an exact version. | [Install the `.deb`](apt.md) |
| macOS / Linux | Homebrew tap | Not yet advanced to the binary formula; install from a verified archive until the first verified bump lands. | [Install with Homebrew](brew.md) |
| Any | Direct install from a verified archive | Available. Verify the release's `SHA256SUMS` with cosign, then unpack the platform archive yourself. | [Install from a verified archive](direct.md) |
| Go-supported platforms | Build from source | Available with the Go version required by `go.mod`. A release-style build fetches the public, digest-pinned local Portal bundle. | [Build from source](build.md) |
| Windows 10/11 | Release .zip asset | Available. Verify the release's SHA256SUMS with cosign, unzip the platform archive, and run the executables directly. | [Install on Windows](windows.md) |
| Any | In-place direct update | Not provided. Use the channel that installed Aplexica; `aplexica update` only reports which channel that is. | [Updating Aplexica](update.md) |

Do not use an unofficial package that happens to use the Aplexica name or
package identifier. Do not execute installer commands copied from unpublished
release material.

## Set up an installed build

After installing from an available package channel, complete the per-user
setup:

```bash
aplexica setup --yes --install
aplexica status
```

This registers and starts the daemon for your user account, installs tray
autostart, and launches the tray when it is enabled (the default on every
supported desktop OS). Run `aplexica setup` without flags for the interactive
walkthrough. Open the local web UI from the tray or with:

```bash
aplexica web open
```

## Linux first run: agent directory permissions

Ubuntu, Mint, Zorin and similar distributions create files with a
group-writable default (umask `002`), so agent directories such as `~/.claude`
and `~/.codex` end up with mode `0775`. Before Aplexica first writes into an
agent's directory it takes a safety snapshot of it, and it refuses to snapshot
a directory that other accounts could write to. That agent is then blocked:
nothing syncs to or from it until the directory is fixed.

`aplexica status` reports the block, naming the agent, the reason and the fix.
Make the directory private to your account, then restart the daemon so it
takes the snapshot again:

```bash
chmod 700 ~/.claude ~/.codex
aplexica daemon restart
```

Apply the same `chmod` to any other directory `aplexica status` names. The
agents themselves run as your account, so removing group and other access does
not affect them.

## Shell completions

Install completions for Bash, Zsh, Fish, or PowerShell with the commands in
[Shell completions](completions.md).

## Release verification

Every release publishes a checksum manifest, its AWS KMS-backed cosign bundle,
and a KMS-backed SLSA v1 provenance bundle. Verify them with the independently
distributed public key and checked-in policy verifier, then check the selected
download against the manifest. The package registry and download host are
transports, never release authority. See [Verify a release](verify.md).

## Registry migration

Devices with a legacy registry must perform the explicit two-phase migration
before the new daemon can use it. Back up the state directory first, stop the
daemon, and follow [Registry v2 to v3 migration](registry-v3-migration.md).

## Uninstalling

Unregister the background services before removing the installed executables:

```bash
aplexica daemon uninstall
aplexica tray uninstall
```

Then use the uninstall section for the channel that installed the binaries:
[Homebrew](brew.md#uninstall), [Debian/Ubuntu](apt.md#uninstall),
[Windows](windows.md#uninstall), or [source build](build.md#uninstall-a-local-build).

Uninstalling the binaries does not remove the canonical store, configuration,
or secrets under `~/.aplexica/` (on Windows,
`%USERPROFILE%\.aplexica\`). Before changing or removing that directory, stop
the daemon and make a backup or archive outside the directory. Verify that the
backup can be opened before discarding any local state. These installation
docs intentionally do not provide a recursive full-data deletion command.
