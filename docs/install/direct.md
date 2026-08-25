# Manual install from a verified archive

This page installs Aplexica from a release archive that you authenticated
yourself. It suits a machine with no supported package manager, an offline or
air-gapped transfer, and anyone who would rather perform the verification step
explicitly than delegate it.

Use the [`.deb`](apt.md) on Debian and Ubuntu if you want dpkg to track the
installed version. For other supported platforms, use this page today:
[WinGet](winget.md) has not been published, and [Homebrew](brew.md) has not yet
been advanced to the verified binary formula. Return to those pages when their
status notices say the channels are available.

Aplexica publishes no installer script. There is no supported `curl | sh`
command, and any note or page offering one is not describing a supported path.
Do not pipe a network response into a shell, and do not treat commands copied
from development notes or old documentation as a working public installer.

## 1. Verify the archive

Download `SHA256SUMS`, `SHA256SUMS.sigstore.json`, and the archive for your
platform; check the cosign signature over `SHA256SUMS`; then check the
archive's digest against that verified list. The exact commands, for both
POSIX shells and PowerShell, are in [Verify a release](verify.md), along with
the full list of published asset names.

Do not continue past a failed check. Preserve the rejected files and report
them using Aplexica's [security policy](../../SECURITY.md).

## 2. Unpack and install

The archive is flat — three executables and five documentation files, with no
top-level directory. Unpack it into a scratch directory: `README.md`,
`LICENSE`, `LICENSE-EXCEPTIONS.md`, `CHANGELOG.md`, and `SECURITY.md` are
archive members too, and extracting into a directory you keep other files in
will overwrite any of those it finds. Unpacking in Finder instead of with
`tar` is safe on that count — Archive Utility puts the members in a new folder
named after the archive, beside it. When the archive is quarantined, Archive
Utility propagates that attribute while the `tar` path does not; see
[macOS signing status and quarantine](#macos-signing-status-and-quarantine).
Substitute that folder for `aplexica-unpack` below if you went that way.

On macOS and Linux:

```bash
VERSION=1.0.70
mkdir -p ~/.local/bin aplexica-unpack
tar -xzf aplexica-$VERSION-darwin-arm64.tar.gz -C aplexica-unpack
install -m 0755 \
  aplexica-unpack/aplexica \
  aplexica-unpack/aplexica-status \
  aplexica-unpack/aplexicatray \
  ~/.local/bin/

# macOS only, and only after step 1 passed: clear the quarantine attribute on
# the installed copies. It succeeds whether or not the attribute is present.
xattr -d -r com.apple.quarantine \
  ~/.local/bin/aplexica \
  ~/.local/bin/aplexica-status \
  ~/.local/bin/aplexicatray 2>/dev/null || true
```

Substitute the archive name for your platform, for example
`aplexica-$VERSION-linux-amd64.tar.gz`. This user-scoped destination avoids
replacing operating-system-managed binaries. Add `~/.local/bin` to your `PATH`
if it is not already there, then open a new shell. The `xattr` line is a no-op
on Linux and after a `tar` extraction on macOS; see
[macOS signing status and quarantine](#macos-signing-status-and-quarantine)
for why it is here rather than further down the page.

On Windows there is no `install` command; expand the `.zip` into a directory
you own and put that directory on your user `PATH`:

```powershell
$Version = '1.0.70'
$Dest = "$env:LOCALAPPDATA\Programs\Aplexica"
New-Item -ItemType Directory -Force -Path $Dest | Out-Null
Expand-Archive -Path "aplexica-$Version-windows-amd64.zip" -DestinationPath $Dest -Force

$UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($UserPath -split ';') -notcontains $Dest) {
  $UserPath = @($UserPath, $Dest | Where-Object { $_ }) -join ';'
  [Environment]::SetEnvironmentVariable('Path', $UserPath, 'User')
}
```

The `PATH` edit is guarded because the Updating section below tells you to
repeat this procedure for every new version, and an unguarded append adds the
same directory again each time. The `Where-Object` filter is there for the same
reason at the other end: appending to an empty or unset user `PATH` with plain
string interpolation leaves a `;;` empty element behind. Note also that
`SetEnvironmentVariable` writes a plain string, so if your user `PATH` contains
unexpanded references such as `%USERPROFILE%\bin` this round trip flattens them
to their current literal values — check the variable first if that matters to
you, and edit it through **Settings → System → About → Advanced system
settings → Environment Variables** instead.

Open a new terminal so the `PATH` change takes effect, and confirm with
`Get-Command aplexica`. Do not use bare `where` for this: in PowerShell `where`
is an alias for `Where-Object`, not the `where.exe` you want, so it prints
nothing and looks like the `PATH` edit failed. Use `where.exe aplexica` if you
prefer that tool. The executables are not Authenticode signed, so SmartScreen
may warn the first time you run one. That warning is expected; the archive
verification in step 1 is what establishes where the bytes came from, not
SmartScreen's silence.

## 3. Complete setup

```bash
aplexica setup --yes --install
aplexica status
```

This registers and starts the daemon with launchd (macOS), `systemctl --user`
(Linux), or a per-user logon scheduled task (Windows), installs tray autostart,
and launches the tray when it is enabled (the default on every supported
desktop OS), and brings up the local web UI. Run `aplexica setup` with no flags
for the interactive walkthrough.

## macOS signing status and quarantine

The macOS executables are **not** Developer-ID signed and **not** notarized.
What `codesign -dvv` reports depends on the architecture, and both answers are
expected:

- **Apple silicon** (`aplexica-<VERSION>-darwin-arm64.tar.gz`) — arm64 macOS
  refuses to execute an unsigned Mach-O, so the Go linker emits an ad-hoc
  signature and `codesign -dvv` reports `flags=0x20002(adhoc,linker-signed)`.
- **Intel** (`aplexica-<VERSION>-darwin-amd64.tar.gz`) — x86_64 macOS has no
  such requirement, so the linker emits nothing and `codesign -dvv` reports
  `code object is not signed at all`. That is the normal result for these
  binaries, not evidence of a damaged or substituted download.

Neither is an Apple-issued status. An ad-hoc signature is self-asserted: it
binds nothing to any identity and Apple never saw it. cosign signs the bytes
of the archive, not the Mach-O inside it, so verifying the archive tells you
where those bytes came from and gives the executables no Apple-issued status
whatsoever.

The practical consequence is quarantine, and where the attribute ends up
decides what you have to do about it. A browser may attach
`com.apple.quarantine` to a downloaded archive; command-line `curl` normally
does not. When an archive does carry the attribute, the two ways of unpacking
it behave differently:

- `tar -xzf` does **not** copy the attribute onto the files it extracts, so
  after the command in step 2 there is nothing on them to clear.
- Finder's Archive Utility **does** copy it onto every extracted file, and
  `install` then carries it forward to the destination. The copies that matter
  are therefore the ones in `~/.local/bin`, not the ones you unpacked.

That is why step 2 clears the attribute on the installed paths, and why it
does so before `aplexica setup` rather than after. Gatekeeper does not report
a quarantined executable carrying no Apple-issued status as a permissions
problem: it kills the process on launch with no message, so a first run that
skipped this step exits
silently with nothing to diagnose. The command is written to succeed on both
paths — `-r` covers the Finder case, and `2>/dev/null || true` keeps the `tar`
case, where the attribute was never set, from reporting an error:

```bash
xattr -d -r com.apple.quarantine \
  ~/.local/bin/aplexica \
  ~/.local/bin/aplexica-status \
  ~/.local/bin/aplexicatray 2>/dev/null || true
```

Clear quarantine only after the verification in step 1 has passed — never
before, and never on a file you could not authenticate.

A Homebrew installation does not set the quarantine attribute at all, which is
one more reason to prefer [the tap](brew.md) on macOS.

## Updating

Repeat the whole procedure for the new version: verify, unpack, install over
the same paths, then restart the daemon so the new binary is the one running.

```bash
aplexica daemon restart
aplexica status
```

`aplexica update --check` reports public release metadata; it does not
authenticate a release or an artifact. The command is advisory on every
installation method: it never downloads a platform archive and never replaces
a file on disk. Follow step 1 yourself before replacing anything. Back up or
archive `~/.aplexica/` (Windows: `%USERPROFILE%\.aplexica\`) before an upgrade;
the state directory is designed to survive one, but a verified backup is the
safer recovery point.

## Uninstall

Unregister the background services while the executables are still present, or
the launchd/systemd unit or scheduled task survives and keeps pointing at a
deleted binary:

```bash
aplexica daemon uninstall
aplexica tray uninstall
```

Then remove only the three executables from the directory you installed them
in. On Windows, delete the directory you expanded the `.zip` into and remove
it from your user `PATH`.

User data under `~/.aplexica/` (Windows: `%USERPROFILE%\.aplexica\`) stays in
place. Back up or archive that directory before deciding whether to discard
any of it, and do not erase the canonical store to work around an installation
problem. See the [general uninstall guidance](_index.md#uninstalling).
