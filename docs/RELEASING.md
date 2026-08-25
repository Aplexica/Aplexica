# Releasing Aplexica

A release starts with one annotated `vX.Y.Z` tag pushed to `origin`.
[`.github/workflows/release.yml`](../.github/workflows/release.yml) validates
the tag, builds the release with GoReleaser, authorizes the checksum manifest
and SLSA v1 provenance with AWS KMS, publishes one GitHub Release, and verifies
the published files. Maintainers do not sign release artifacts locally.

> [!IMPORTANT]
> The first automated release is blocked until the repository contains the
> reviewed trust anchor `aplexica-release.pub` and CI proves that it is the
> public key exported from the release KMS key. Do not cut a release, publish
> an unpinned key, or invent a fingerprint to work around this requirement.

## Release contract

### Assets

Platform archives follow this template:

```text
aplexica-<VERSION>-<GOOS>-<GOARCH>.<EXT>
```

`VERSION` is the tag without its leading `v`. `GOOS` is `darwin`, `linux`, or
`windows`; `GOARCH` is `amd64` or `arm64`. Windows uses `zip`; the other
platforms use `tar.gz`.

Every release contains exactly these thirteen assets:

```text
aplexica-X.Y.Z-darwin-amd64.tar.gz
aplexica-X.Y.Z-darwin-arm64.tar.gz
aplexica-X.Y.Z-linux-amd64.tar.gz
aplexica-X.Y.Z-linux-arm64.tar.gz
aplexica-X.Y.Z-windows-amd64.zip
aplexica-X.Y.Z-windows-arm64.zip
aplexica_X.Y.Z_amd64.deb
aplexica_X.Y.Z_arm64.deb
aplexica-X.Y.Z-source.tar.gz
aplexica.sbom.cdx.json
SHA256SUMS
SHA256SUMS.sigstore.json
aplexica.provenance.sigstore.json
```

The `.deb` filenames deliberately use underscores and Debian architecture
names. The SBOM, checksum file, and cosign bundles deliberately have stable,
unversioned names. Do not harmonize either exception.

The six platform archives are flat and contain exactly:

```text
aplexica              # aplexica.exe on Windows
aplexica-status       # aplexica-status.exe on Windows
aplexicatray          # aplexicatray.exe on Windows
CHANGELOG.md
LICENSE
LICENSE-EXCEPTIONS.md
README.md
SECURITY.md
```

The source archive is different: it has the top-level directory
`aplexica-X.Y.Z/` so extracting it cannot overwrite unrelated files in the
current directory.

`SHA256SUMS` authenticates the six platform archives, two Debian packages,
source archive, and SBOM. It does not list itself or either signature bundle.
The provenance statement has the same ten files as subjects.

### Signing and verification

One KMS public key authorizes two cosign signatures: a message signature over
`SHA256SUMS` and a DSSE signature over the SLSA v1 statement. The exact signing
commands are:

```bash
cosign sign-blob --yes \
  --key "$COSIGN_KMS_URI" \
  --bundle signed-release/SHA256SUMS.sigstore.json \
  signed-release/SHA256SUMS

cosign attest-blob --yes \
  --key "$COSIGN_KMS_URI" \
  --type slsaprovenance1 \
  --statement "$RUNNER_TEMP/aplexica.provenance.json" \
  --bundle signed-release/aplexica.provenance.sigstore.json
```

`$COSIGN_KMS_URI` is a CI-only signing input. It must never appear in a public
verification command. Public verification uses the reviewed repository trust
anchor `aplexica-release.pub`; it must not use an AWS URI, cloud credential, or
certificate identity.

The public key is not a release asset: placing a key beside the signature it
purports to authenticate would make the release self-trusting. Before using
the following commands, use the reviewed Aplexica source tree that contains
`aplexica-release.pub` and the provenance policy verifier, checked out at the
release tag. The independent key-distribution location cannot be documented
until the real key exists.

Export `VERSION=X.Y.Z` first. The following block is the canonical
macOS/Linux verification procedure. Keep
it byte-identical to the corresponding block in
[`docs/install/verify.md`](install/verify.md) and to the cryptographic command
run by the workflow's `verify` job.

```bash
bash -eu -o pipefail <<'VERIFY'
: "${VERSION:?set VERSION to the release version without v}"
BASE="https://github.com/Aplexica/Aplexica/releases/download/v$VERSION"
ASSET="aplexica-$VERSION-darwin-arm64.tar.gz"

curl -fLO "$BASE/SHA256SUMS"
curl -fLO "$BASE/SHA256SUMS.sigstore.json"
curl -fLO "$BASE/aplexica.provenance.sigstore.json"
curl -fLO "$BASE/$ASSET"

cosign verify-blob \
  --key aplexica-release.pub \
  --bundle SHA256SUMS.sigstore.json \
  SHA256SUMS

awk -v asset="$ASSET" '
  $2 == asset { matches++ }
  END { exit matches == 1 ? 0 : 1 }
' SHA256SUMS

shasum -a 256 --check --ignore-missing SHA256SUMS

cosign verify-blob-attestation \
  --type slsaprovenance1 \
  --key aplexica-release.pub \
  --bundle aplexica.provenance.sigstore.json \
  "$ASSET"

go run -mod=readonly ./tools/releaseprovenance \
  --verify-bundle aplexica.provenance.sigstore.json \
  --checksums SHA256SUMS \
  --commit "$(git rev-parse HEAD)" \
  --portal-release packaging/portal-release.json \
  --repository Aplexica/Aplexica \
  --ref "refs/tags/v$VERSION"
VERIFY
```

Both cosign commands, the exact-name assertion, the checksum command, and the
checked-in provenance policy verifier must succeed.

## Prerequisites

Do not create the tag until all of these statements are true:

- `aplexica-release.pub` exists in its reviewed repository location, and CI
  byte-compares it with the public key exported from the configured KMS key.
- The release workflow can anonymously fetch every pinned public build input
  and invoke only the required KMS signing operations.
- The release commit is reviewed, pushed, and contains no uncommitted release
  changes.
- [`internal/version/version.go`](../internal/version/version.go) contains
  `vX.Y.Z`, and [`CHANGELOG.md`](../CHANGELOG.md) starts with the matching
  `## [X.Y.Z] - YYYY-MM-DD` section.
- [`packaging/portal-release.json`](../packaging/portal-release.json) pins a
  portal bundle that CI can fetch and validate.
- All required repository tests, policy checks, packaging checks, and release
  dry runs are green for the exact commit being tagged.

## Cut a release

1. Prepare the release commit:

   - update `internal/version/version.go`;
   - add the matching top section to `CHANGELOG.md`;
   - update `packaging/portal-release.json` only when the embedded portal
     changes; and
   - run the required repository gates.

2. Commit and push those changes. The tag must point at the reviewed commit,
   not at an uncommitted working tree.

3. Create and push an annotated tag:

   ```bash
   git tag -a vX.Y.Z -m "Aplexica vX.Y.Z"
   git push origin vX.Y.Z
   ```

   The train accepts only `vMAJOR.MINOR.PATCH`. It does not publish prerelease
   suffixes.

4. Watch the `Release` workflow through completion. The expected order is:

   1. validate the annotated tag, source version, and changelog;
   2. fetch and validate pinned build inputs;
   3. build the release candidate with GoReleaser;
   4. confirm `SHA256SUMS` accounts for every hashed asset;
   5. export and compare the KMS public key with `aplexica-release.pub`;
   6. sign `SHA256SUMS` and the allowlisted SLSA v1 statement;
   7. verify both bundles locally without KMS;
   8. publish the 13-asset GitHub Release; and
   9. download and verify the published files with
      `aplexica-release.pub`.

   Signing must finish before publication. Verification runs after publication,
   so a failure in the verify job means a release is already visible and must
   be yanked.

5. Verify the release again from a clean directory using the canonical block
   above. Do not advance a package channel from an unverified checksum file.

## Package channels

GitHub Releases are the authoritative distribution channel. Other channels
consume only digests from a successfully verified `SHA256SUMS`.

- **GitHub Releases:** published by the tag-triggered release workflow.
- **Homebrew:** the binary formula may advance only after release verification.
  Its four archive digests come from the verified checksum file.
- **WinGet:** paused. The release workflow must not submit or update WinGet
  manifests.
- **APT:** no repository is published. Users install an exact `.deb` downloaded
  from a GitHub Release.

The Homebrew formula source is
[`packaging/homebrew/aplexica.rb`](../packaging/homebrew/aplexica.rb). When
Homebrew publication is enabled, it remains downstream of the verify job and
renders the formula from verified digests. A release remains valid when the
Homebrew channel is not advanced; the GitHub Release is still authoritative.

WinGet remains a separate, manual channel until its first accepted package and
an independently reviewed publication design exist. Do not add WinGet
publication to the release workflow as part of an ordinary release.

There is no APT index to regenerate. The two `.deb` files are release assets,
and [`docs/install/apt.md`](install/apt.md) documents installing them directly.

## Recovery

First determine whether `gh release create` completed. The response differs
before and after publication.

### Failure before publication

If the source tree is correct, no release object exists, and signing never
started, rerun the failed workflow for the same tag. Do not move the tag merely
to obtain another run.

If the tag points at the wrong commit and the signing step never started,
remove the unpublished tag, correct the release commit, and create the
annotated tag again:

```bash
git push origin :refs/tags/vX.Y.Z
git tag -d vX.Y.Z
git tag -a vX.Y.Z -m "Aplexica vX.Y.Z"
git push origin vX.Y.Z
```

If signing started, treat the version as consumed even when publication failed.
Fix the problem and cut a higher patch version instead of rerunning or moving
the signed tag.

If an incomplete release object or draft remains, delete it before deciding
whether the tag is safe to retry:

```bash
gh release delete vX.Y.Z --repo Aplexica/Aplexica --cleanup-tag --yes
git tag -d vX.Y.Z
```

### Failure after publication

Never replace, add, or delete individual assets in place. Never reuse or move a
published tag. Yank the release and publish the correction under a higher
version.

Delete the release and remote tag together, then remove the local tag:

```bash
gh release delete vX.Y.Z --repo Aplexica/Aplexica --cleanup-tag --yes
git tag -d vX.Y.Z
```

If Homebrew already advanced, revert the formula to the previous verified
version and push that revert to the tap. Confirm the published formula, not
merely a local checkout, names the previous version. WinGet is paused and APT
has no repository index, so neither has automated state to unwind.

A yank stops further distribution; it is not a recall. Existing downloads can
still verify, fetched tags remain in downstream clones, and any external
transparency evidence remains outside the repository. The remedy is always a
new, higher release containing the fix.

## Invariants

- Release artifacts are signed only by CI. Never sign them locally and never
  ask another maintainer to sign them locally.
- The KMS URI is used only for signing. Verification always uses the pinned
  `aplexica-release.pub` trust anchor.
- Both KMS signatures precede `gh release create`; published verification
  follows it.
- Release notes come from the matching `CHANGELOG.md` section.
- Package metadata is derived only from a verified `SHA256SUMS`.
- A published version is immutable. Corrections always receive a higher
  version.
- WinGet publication remains paused.
