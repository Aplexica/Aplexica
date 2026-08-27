# Install on Windows (10/11)

Aplexica is not installed through WinGet: a submission to the Microsoft
community repository exists, but it is not the supported install path.

## Download

From the [v1.0.74 release](https://github.com/Aplexica/Aplexica/releases/tag/v1.0.74),
download:

- `aplexica-1.0.74-windows-amd64.zip` for Intel/AMD PCs
- `aplexica-1.0.74-windows-arm64.zip` for Windows on ARM

Verify the release's `SHA256SUMS` with cosign and check the zip's digest with
`Get-FileHash`. The exact commands are in [verify.md](verify.md). Do not
continue past a failed check.

## Unzip

Right-click the zip, choose **Extract All**, and extract into a folder you own,
for example `%LOCALAPPDATA%\Programs\Aplexica`. The archive is flat:
`aplexica.exe`, `aplexica-status.exe`, `aplexicatray.exe`, plus five
documentation files (`README.md`, `LICENSE`, `LICENSE-EXCEPTIONS.md`,
`CHANGELOG.md`, `SECURITY.md`).

## Run

From that folder run `.\aplexica.exe --version` and `.\aplexica.exe setup`
(CLI), or double-click `aplexicatray.exe` (tray). The executables are not
Authenticode signed, so SmartScreen may warn the first time you run one. That
warning is expected; the archive verification in Download is what authenticates
the bytes.

## One-shot setup (optional)

```powershell
.\aplexica.exe setup --yes --install
```

This registers the daemon's per-user logon Scheduled Task named `Aplexica Sync
Daemon`, writes the Startup shortcut `Aplexica Tray.lnk`, starts both, and
opens the local web UI. This is setup, not an install channel. Unzip above is
the install.

## Start the tray at logon

`aplexicatray.exe` does not write a Startup shortcut when you launch it.

1. Press `Win+R`, type `shell:startup`, press Enter. This opens
   `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup`.
2. Right-click inside the folder, **New > Shortcut**, and browse to
   `aplexicatray.exe` in the unzip folder.
3. In the shortcut's Properties, set **Start in** (working directory) to the
   unzip folder.

`aplexica tray install` writes the equivalent shortcut (`Aplexica Tray.lnk` in
the same Startup folder). Both approaches are the same mechanism. Do not add a
`.bat` or `.ps1` file.

## Start the daemon at logon

```powershell
.\aplexica.exe daemon install --dir <path-to-watch>
```

This registers the `Aplexica Sync Daemon` per-user Scheduled Task. A Startup
shortcut to the tray does **not** start the daemon. The tray only offers a
manual Start item in its menu when the daemon is down.

## Optional PATH

Adding the unzip folder to the user PATH is optional. See [direct.md](direct.md)
for the guarded PowerShell PATH edit.

## Updating

Repeat download, verify, and unzip for the new version over the same folder,
then `aplexica daemon restart`. See [Updating Aplexica](update.md).

## Uninstall

Unregister the background services **before** deleting the binaries, or the
scheduled task and Startup shortcut survive and point at a deleted executable:

```powershell
aplexica daemon uninstall
aplexica tray uninstall
```

Then delete the unzip folder and remove it from PATH if you added it. User data
under `%USERPROFILE%\.aplexica\` stays. See the
[general uninstall guidance](_index.md#uninstalling).
