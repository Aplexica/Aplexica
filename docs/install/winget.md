# Install via winget (Windows 10/11)

> **Not yet published:** the `Aplexica.Aplexica` package has never been
> submitted to the WinGet community repository, so the install command below
> fails with “No package found matching input criteria.” Until the package
> is live, install from the Windows release archive — see
> [Install from a verified archive](direct.md). The rest of this page
> describes the package as it will behave once it is published.

## Install

```powershell
winget install Aplexica.Aplexica
```

This installs `aplexica.exe`, `aplexica-status.exe`, and `aplexicatray.exe` as
portable commands. The local web UI is compiled into `aplexica.exe`; there is
nothing extra to install.

winget's portable installer type runs **no** post-install step, so complete
setup yourself — this is the step that actually gets you a running daemon:

```powershell
aplexica setup --yes --install
aplexica status
```

That installs the daemon as a per-user logon scheduled task ("Aplexica Sync
Daemon"), installs tray autostart, starts the daemon, and brings up the local
web UI. Run `aplexica setup` with no flags for an interactive walkthrough.

Open the UI from the tray icon → **Open Aplexica**, or:

```powershell
aplexica web open
```

## Upgrade

```powershell
winget upgrade Aplexica.Aplexica
```

Or `winget upgrade --all` to upgrade everything pending. Your configuration and
data under `%USERPROFILE%\.aplexica\` are preserved. Restart the daemon after
upgrading so the new binary is running:

```powershell
aplexica daemon restart
```

## Uninstall

Unregister the background services **before** removing the binaries, or the
scheduled task and tray autostart survive and point at a deleted executable:

```powershell
aplexica daemon uninstall
aplexica tray uninstall
winget uninstall Aplexica.Aplexica
```

User data under `%USERPROFILE%\.aplexica\` is left in place. Before changing or
removing that directory, stop the daemon and make a backup or archive outside
it; verify that the backup can be opened before discarding any local state. See
the [general uninstall guidance](_index.md#uninstalling).

## Architecture support

Both `x64` and `arm64` builds are published, as
`aplexica-<VERSION>-windows-amd64.zip` and
`aplexica-<VERSION>-windows-arm64.zip`. The WinGet manifest's
per-architecture digests are transcribed from the release's cosign-verified
`SHA256SUMS`; nothing else authenticates them on the way in.

## Release authentication

The Windows archives are listed in the release's `SHA256SUMS`, exactly like
the macOS and Linux artifacts, and its cosign signature is authorized by the
release AWS KMS key. The independently distributed `aplexica-release.pub` is
the release trust anchor.

The executables are not Authenticode signed, so SmartScreen may warn the
first time you run one. Authenticate the archive instead of trusting the
warning's absence: see [verify.md](verify.md) for the exact
`cosign verify-blob` command and the PowerShell `Get-FileHash` form of the
digest check.

## Troubleshooting

### `aplexica` not on PATH

WinGet places command shims in its links directory
(`%LOCALAPPDATA%\Microsoft\WinGet\Links` for a user-scope install) and adds
that directory to PATH; the extract directory itself is not added. If your
shell was open before install, close and reopen it. Verify with
`where aplexica`.

### Tray doesn't appear

The tray is a separate process (`aplexicatray.exe`). Setup launches it once and
creates a Windows Startup shortcut so Windows launches it at the next logon.
Inspect with:

```powershell
Get-Process aplexicatray
aplexica setup --yes --install
```

If you opted out, re-enable it by re-running `aplexica setup --install` with tray enabled.

### Daemon won't start because of firewall prompt

The daemon binds to **127.0.0.1 (loopback)** only — there is no LAN listener,
so it accepts local inbound HTTP connections but no connection from another
machine. Denying a public-network firewall rule does not affect the loopback
UI. Outbound network access happens only on an explicit action such as
`aplexica update`, or through a remote plugin you configured yourself.
