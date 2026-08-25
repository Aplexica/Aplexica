# winget publishing

Package ID: **`Aplexica.Aplexica`** in [microsoft/winget-pkgs](https://github.com/microsoft/winget-pkgs).

## Status

**Not yet published.** The package has never been submitted, so
`winget install Aplexica.Aplexica` fails with "No package found matching
input criteria". Until the package is live, the Windows install path is the
manual download-and-verify route documented in `docs/install/direct.md`.

## First submission (when WinGet promotion resumes)

The first version requires a manual PR. Update automation is not configured,
and tools such as
[winget-releaser](https://github.com/vedantmgoyal9/winget-releaser) can update
only packages that already exist in winget-pkgs:

1. Pick the newest stable release (vX.Y.Z) with `aplexica-X.Y.Z-windows-amd64.zip`
   and `-arm64.zip` assets. Download that release's `SHA256SUMS` and
   `SHA256SUMS.sigstore.json`, verify the list with `cosign verify-blob`
   against the independently distributed `aplexica-release.pub` public key —
   the exact command is in `docs/install/verify.md` — and only then read the
   two zip digests out of it. Digests from an unverified `SHA256SUMS` are worth
   nothing.
2. Fill the three templates in this directory (`VERSION_PLACEHOLDER`,
   `RELEASE_DATE_PLACEHOLDER`, `SHA256_WIN_AMD64`, `SHA256_WIN_ARM64`) into
   `manifests/a/Aplexica/Aplexica/X.Y.Z/` of a winget-pkgs fork.
3. Validate on a Windows machine:
   ```powershell
   winget validate --manifest <dir>
   # Optional local install test (requires: winget settings → enable LocalManifestFiles)
   winget install --manifest <dir>
   ```
4. Open the PR against microsoft/winget-pkgs. Expect automated validation and
   human moderation; timing varies. `komac submit` or `wingetcreate new` can
   automate steps 2–4.

## Hosted updates are quarantined

Still quarantined, for a restated reason. No source-controlled workflow
submits winget updates or holds a winget-pkgs publishing token:
`.github/workflows/release.yml` publishes GitHub Release assets and stops
there, deliberately without a winget job. An operator may submit or update
the manifest only after verifying that release's `SHA256SUMS` with
`cosign verify-blob` (see `docs/install/verify.md`) and transcribing the two
zip digests out of the cosign-verified list.

## Contract with the release build

GoReleaser, driven by `.goreleaser.yaml` from the release workflow, produces
exactly what the installer manifest declares:
`aplexica-<VERSION>-windows-{amd64,arm64}.zip` (bare version, no leading `v`)
containing `aplexica.exe`, `aplexica-status.exe`, `aplexicatray.exe` at the
archive root — flat, no top-level directory, which is exactly what the
manifest's `NestedInstallerFiles` entries assume (portable, per-user, PATH
aliases). If archive naming or contents change, update
`Aplexica.Aplexica.installer.yaml` and `.goreleaser.yaml` together.
