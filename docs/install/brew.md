# Install with Homebrew

> **Tap not yet bumped:** until the tap is advanced to the reviewed binary
> formula in `packaging/homebrew/aplexica.rb`, install from the macOS or Linux
> release archive — see
> [Install from a verified archive](direct.md). The rest of this page
> describes the tap as it will behave once it is bumped: the live v1.0.41
> formula builds from a pinned git revision and records no archive `sha256`.

Once the tap is bumped, Homebrew is the simplest path on macOS and Linux.

```bash
brew install Aplexica/tap/aplexica
```

That installs three executables from the published release archive:

- `aplexica` — CLI, daemon, and local web UI server
- `aplexica-status` — status helper the tray spawns
- `aplexicatray` — menu bar / system-tray indicator

The archive is covered by `SHA256SUMS`, whose cosign signature is authorized by
the release AWS KMS key. The macOS executables inside it are **not** Developer-ID
signed and **not** notarized: on Apple silicon they carry only the ad-hoc
signature the Go linker is obliged to emit, and on Intel they carry no
signature at all. cosign signs the archive bytes, not the Mach-O. See
[macOS signing status and quarantine](direct.md#macos-signing-status-and-quarantine)
for what `codesign -dvv` reports and why.

The local web UI is compiled into `aplexica` itself; there is nothing extra to
install.

Then bring everything up:

```bash
aplexica setup --yes --install
aplexica status
```

This registers the daemon with launchd (macOS) or `systemctl --user` (Linux),
installs tray autostart, starts the daemon, and brings up the local web UI.
Run `aplexica setup` with no flags for an interactive walkthrough.

Open the UI by clicking the tray icon → **Open Aplexica**, or:

```bash
aplexica web open
```

## Do not use `brew services`

`aplexica setup --install` already registers the daemon with your platform's
service manager. Starting a second supervisor with `brew services start
aplexica` would fight it for the same process. The formula deliberately ships
no `service` block.

## Upgrade

```bash
brew update && brew upgrade aplexica
```

Your configuration and canonical store under `~/.aplexica/` are preserved.
After a daemon upgrade, restart it so the new binary is running:

```bash
aplexica daemon restart
```

## Linux tray

The tray uses the StatusNotifierItem protocol over DBus — no GTK or
AppIndicator development libraries are needed. On GNOME you need the
AppIndicator/AppIndicatorSupport shell extension for the icon to appear;
`aplexica web open` works regardless.

## Verifying what Homebrew installed

Homebrew checks the archive against the `sha256` recorded in the formula, and
that value is transcribed from the release's cosign-verified `SHA256SUMS`.
That is channel integrity: it proves the tap and the release agree, not that
the release is authentic.

To authenticate the release yourself, follow
[Verify a release](verify.md). The tarball Homebrew downloaded is the same
artifact `SHA256SUMS` covers, so its digest must match the manifest line for
your platform's `aplexica-<VERSION>-<GOOS>-<GOARCH>.tar.gz`.

The trust anchor is `aplexica-release.pub` verifying the KMS-authorized cosign
bundle, never Homebrew, GitHub, or any registry. A tap is a byte transport.

## Aplexica Cloud

Aplexica Cloud is a separate commercial component. **No Homebrew, apt, winget,
or direct-download channel ships `aplexica-cloud-plugin`.** The OSS daemon only
*authorizes* which plugin versions it will load. If you have a plugin, enroll
it with `aplexica setup --yes --install --cloud <absolute path>` plus its
out-of-band trust values; run `aplexica setup --help` for the exact form. On
macOS the plugin tree must be root-owned and read-only — user-owned and
Homebrew-prefix paths fail closed by design.

## Uninstall

Unregister the background services **before** removing the formula, or the
launchd/systemd service and the tray autostart entry survive and keep pointing
at a deleted binary:

```bash
aplexica daemon uninstall
aplexica tray uninstall
brew uninstall aplexica
brew untap Aplexica/tap
```

User data under `~/.aplexica/` is left in place, including your canonical
store and conversation history. Back up or archive that directory before
deciding whether to discard any data; see the
[general uninstall guidance](_index.md#uninstalling).
