# Updating Aplexica

Update Aplexica through the same channel that installed it. `aplexica update`
never replaces the running executable. It reports which installer owns the
build you are running; for an unclaimed official release build, it can also
report the newest complete GitHub release and print manual upgrade steps.

Before an upgrade, stop active work and back up or archive `~/.aplexica/`
(Windows: `%USERPROFILE%\.aplexica\`). Package upgrades are designed to retain
that state, but a verified backup is the safer recovery point.

## Homebrew

Update the tap and upgrade Aplexica, then restart the daemon so the new binary
is running:

```bash
brew update
brew upgrade aplexica
aplexica daemon restart
```

The public tap has not been bumped to a binary formula yet, so this path is
not usable today — see [Install with Homebrew](brew.md) for the current status.

## Debian and Ubuntu

There is no signed `.deb` for v1.0.69 and no official Aplexica APT repository.
Do not install an unlisted package or add a third-party repository. Existing
`.deb` users should keep their current installation until a signed package
upgrade is published, or back up their state and deliberately migrate to one of
the supported paths documented in [Debian/Ubuntu package status](apt.md).

## Source builds

Build the selected tag in a fresh checkout with Go 1.25.12, run the test suite,
then replace only the user-scoped executables you installed previously. Follow
[Build from source](build.md) and unregister/re-register services if the binary
location changes.

## Windows (release archive)

Repeat the download-verify-unzip procedure for the new version over the same
folder per [Install on Windows](windows.md), then restart the daemon so the new
binary is running:

```powershell
aplexica daemon restart
```

## What `aplexica update` does

`aplexica update` is advisory only. It never downloads a platform archive,
never replaces a file on disk, and never restarts anything. It first works out
which installer owns the executable you are running, and then takes one of two
paths.

**If a package manager owns it** — Homebrew, apt/dpkg, or WinGet — it stops
there without contacting GitHub or verifying a signature. A source build
short-circuits the same way; see [Source builds](#source-builds) above. Because
all three package-manager channels are currently unavailable, the command
explains that status instead of printing an upgrade command.

**Only for an official release build that no package manager claims** does it
read the public GitHub API's latest-release metadata. It accepts only a stable
`vMAJOR.MINOR.PATCH` tag and requires the metadata to list `SHA256SUMS`,
`SHA256SUMS.sigstore.json`, and `aplexica.provenance.sigstore.json` before it
prints the manual download-verify-replace recipe. This is availability
discovery, not authentication: the command downloads none of those files and
verifies no signature or digest. Authenticate the downloaded files yourself by
following [Verify a release](verify.md).

Nothing runs automatically. Follow the linked channel instructions or the
manual steps the command prints.

Use `--check` to report without prompting, and `--json` for a machine-readable
result.

These are the package-manager commands associated with each channel:

```bash
brew upgrade aplexica
sudo apt update && sudo apt install --only-upgrade aplexica
```

The updater prints none of these commands while the corresponding channel is
unavailable. The APT form also assumes the package came from a repository.
There is no official Aplexica APT repository yet, so a manually installed
`.deb` is upgraded by installing the newer `.deb` over it — see
[Debian and Ubuntu](#debian-and-ubuntu) above.

A hand-placed official release binary is the case with no package-manager
command to print. It gets the manual branch described above — the new version,
the archive URL, three evidence URLs, and a pointer to
[Verify a release](verify.md), which you follow before replacing anything.

A source build is different again: `aplexica update` does not resolve a release at
all. It reports that the checkout owns the binary, points at
[Build from source](build.md), and exits 20 without contacting GitHub.

Exit codes:

| Code | Meaning |
|---|---|
| 0 | Success. No update available, or the upgrade steps were printed. |
| 1 | Operational failure (network, parse, or filesystem). |
| 2 | Confirmation required — re-run with `--yes` — or the prompt was declined. |
| 3 | Security. The discovered release is below this machine's local rollback floor. `--allow-downgrade` deliberately waives that comparison. |
| 10 | A complete release is listed in GitHub metadata. Returned by `--check`; this is a status, not a failure. Downloaded bytes have not been authenticated. |
| 20 | Delegated. A package manager owns this build, or it is a source build; follow the linked channel instructions or rebuild the checkout. |
| 21 | Reserved for a discovery implementation that requires separate bootstrap state. The built-in public GitHub metadata path does not emit this code. |
| 22 | Ambiguous ownership. Two package managers both claim this executable; resolve that before upgrading. An executable no manager claims is not an error — it gets the manual recipe. |

A script wrapping `aplexica update --check` must treat **10** as the normal
"update available" outcome rather than as an error. Code **3** reports only a
local rollback-floor refusal; it does not mean that `aplexica update` checked a
release signature.

## Rollback floor

`aplexica update` keeps a highest-version-ever-run watermark at
`<StateDir>/update-floor`, a single `X.Y.Z` line. The floor records the
version of the executable that is running, not the version discovered on
GitHub, so checking for an update you never install does not raise it. A
discovered release below that line is refused with exit code 3. Pass
`--allow-downgrade` when you deliberately want an older version; it relaxes
only this local comparison and does not authenticate any downloaded file.

This is local state, not a signed predecessor chain. It protects a machine
that has already run a newer release; it protects nothing on a fresh
install, where there is no recorded floor to compare against.
