# Verify a release

Every automated Aplexica release carries two AWS KMS-authorized cosign
bundles made with the same non-exportable signing key:

1. `SHA256SUMS.sigstore.json` signs `SHA256SUMS`, which lists the SHA-256
   digest of every executable archive, Debian package, source archive, and
   SBOM.
2. `aplexica.provenance.sigstore.json` signs an in-toto statement whose ten
   subjects are those same files and whose predicate uses the SLSA provenance
   v1 schema.

The release download host is not the trust root. Verification uses the
independently distributed public key `aplexica-release.pub`, and cosign also
checks the public transparency evidence in each bundle. Do not unpack or run a
file if any command below fails.

## Release authority

The private key is an asymmetric AWS KMS key. It cannot be exported to this
repository, a GitHub runner, or a maintainer workstation. A release job receives
a short-lived AWS session through GitHub OIDC, but AWS KMS performs the signing
operation.

The public trust anchor is the exact PKIX PEM file:

```text
aplexica-release.pub
```

Obtain it from the stable Aplexica key-distribution location documented with
the first KMS-signed release. Do not download a public key from the release you
are trying to verify and then trust it merely because it was beside the
signature. The first KMS-signed release is blocked until the key, its SHA-256
fingerprint, and its rotation procedure have been published independently.

The KMS signature does not contain a GitHub certificate identity or a release
tag. The verification procedure therefore requires exactly one checksum entry
for the versioned asset name you selected. That check prevents a valid manifest
from an older release from being replayed beside a newer tag.

## Install cosign

Install cosign v3.1.1 or a reviewed compatible version. Homebrew provides it as
`cosign`. On Windows, download `cosign-windows-amd64.exe` from the
[Sigstore releases page](https://github.com/sigstore/cosign/releases), rename
it to `cosign.exe`, and put it on `PATH`. Sigstore does not publish a Windows
ARM64 executable; Windows on ARM can run the AMD64 build under emulation.

## Verify on macOS or Linux

Run from the reviewed Aplexica source tree that supplied
`aplexica-release.pub` and the checked-in provenance policy verifier. Check out
the same release tag, then export the version without its leading `v`:

```bash
export VERSION=1.0.70
```

Run this block unchanged. It is also executed by release CI after publication:

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

Change `ASSET` inside the block for another platform or for the source archive.
All five conditions must pass:

- the checksum manifest has a valid KMS signature and transparency evidence;
- the signed manifest contains exactly one entry for the requested filename;
- the downloaded bytes match that entry; and
- the file is a subject of the KMS-signed SLSA v1 provenance statement;
- the signed statement passes Aplexica's exact public builder, tag, subject,
  dependency, and JSON-field policy.

The cosign selector is `slsaprovenance1` deliberately. In cosign v3.1.1 the
shorter `slsaprovenance` name means deprecated SLSA provenance v0.2.

## Verify on Windows

Run from the reviewed Aplexica source tree at the same release tag. It must
contain `aplexica-release.pub`, `tools/releaseprovenance`, and
`packaging/portal-release.json`. In a clean PowerShell session rooted there,
run:

```powershell
$ErrorActionPreference = 'Stop'
$Version = '1.0.70'
$Base = "https://github.com/Aplexica/Aplexica/releases/download/v$Version"
$Archive = "aplexica-$Version-windows-amd64.zip"
$Commit = (git rev-parse HEAD).Trim()

if (-not (Get-Command cosign -ErrorAction SilentlyContinue)) {
  throw 'cosign is not on PATH; nothing has been verified.'
}

Invoke-WebRequest -Uri "$Base/SHA256SUMS" -OutFile SHA256SUMS
Invoke-WebRequest -Uri "$Base/SHA256SUMS.sigstore.json" -OutFile SHA256SUMS.sigstore.json
Invoke-WebRequest -Uri "$Base/aplexica.provenance.sigstore.json" -OutFile aplexica.provenance.sigstore.json
Invoke-WebRequest -Uri "$Base/$Archive" -OutFile $Archive

cosign verify-blob `
  --key aplexica-release.pub `
  --bundle SHA256SUMS.sigstore.json `
  SHA256SUMS
if ($LASTEXITCODE -ne 0) {
  throw "checksum signature verification failed with exit code $LASTEXITCODE"
}

$Hash = (Get-FileHash -Algorithm SHA256 $Archive).Hash.ToLowerInvariant()
$Matches = @(Get-Content SHA256SUMS | Where-Object { $_ -eq "$Hash  $Archive" })
if ($Matches.Count -ne 1) {
  throw "$Archive must match exactly one SHA256SUMS entry"
}

cosign verify-blob-attestation `
  --type slsaprovenance1 `
  --key aplexica-release.pub `
  --bundle aplexica.provenance.sigstore.json `
  $Archive
if ($LASTEXITCODE -ne 0) {
  throw "provenance verification failed with exit code $LASTEXITCODE"
}

go run -mod=readonly ./tools/releaseprovenance `
  --verify-bundle aplexica.provenance.sigstore.json `
  --checksums SHA256SUMS `
  --commit $Commit `
  --portal-release packaging/portal-release.json `
  --repository Aplexica/Aplexica `
  --ref "refs/tags/v$Version"
if ($LASTEXITCODE -ne 0) {
  throw "provenance policy verification failed with exit code $LASTEXITCODE"
}

$Matches[0]
```

PowerShell does not automatically stop when a native command returns nonzero,
which is why both cosign exit codes are checked explicitly.

## Release assets

Every automated release publishes exactly thirteen assets. Asset filenames use
the version without the tag's leading `v`:

```text
aplexica-1.0.70-darwin-amd64.tar.gz
aplexica-1.0.70-darwin-arm64.tar.gz
aplexica-1.0.70-linux-amd64.tar.gz
aplexica-1.0.70-linux-arm64.tar.gz
aplexica-1.0.70-windows-amd64.zip
aplexica-1.0.70-windows-arm64.zip
aplexica_1.0.70_amd64.deb
aplexica_1.0.70_arm64.deb
aplexica-1.0.70-source.tar.gz
aplexica.sbom.cdx.json
SHA256SUMS
SHA256SUMS.sigstore.json
aplexica.provenance.sigstore.json
```

`SHA256SUMS` covers the first ten files. It does not list itself or either
signature bundle. The provenance statement also has exactly the first ten files
as subjects; its bundle cannot contain its own digest without creating a cycle.

Each platform archive is flat and contains exactly:

```text
aplexica            # aplexica.exe on Windows
aplexica-status     # aplexica-status.exe on Windows
aplexicatray        # aplexicatray.exe on Windows
CHANGELOG.md
LICENSE
LICENSE-EXCEPTIONS.md
README.md
SECURITY.md
```

The source archive instead has one top-level `aplexica-<VERSION>/` directory.

## What the provenance says

The provenance is Aplexica-authored and KMS-authorized. It uses the in-toto
Statement v1 envelope and the SLSA provenance v1 predicate schema. It records
only public release facts:

- the ten artifact names and SHA-256 digests;
- `Aplexica/Aplexica`, the tag ref, source commit, and release workflow path;
- the public GitHub Actions run URL; and
- the public, digest-pinned local Portal release asset embedded in the daemon.

It does not contain environment dumps, secrets, token names or values, actor
identities, AWS account or resource identifiers, hostnames, local paths, private
repository names, commands, or dependency inventories. The same workflow builds
the artifacts and authors this statement, so Aplexica does not claim an
independently generated provenance statement or a SLSA build level.

Optional Homebrew promotion happens after the release is built, signed,
published, and verified. It changes no release artifact and is outside this
build-provenance boundary.

`cosign verify-blob-attestation` verifies the signature, transparency evidence,
predicate type, and that the supplied file's digest appears as a subject. The
checked-in verifier then requires canonical deterministic JSON, the exact ten
manifest subjects, the expected public builder/source/tag binding, and the
digest-pinned public Portal dependency while rejecting every unknown field.
Release CI runs both checks before and after publication.

## Platform signatures

The executables are not Apple Developer-ID signed, notarized, or Authenticode
signed. Gatekeeper and SmartScreen can therefore warn even after cosign
verification succeeds. Cosign authenticates the downloadable archive and its
contents by digest; it does not change platform code-signing status. See
[Manual install from a verified archive](direct.md).

## Package channels

- Homebrew formulas pin archive digests copied only from a verified
  `SHA256SUMS`.
- WinGet publication is paused. When enabled, its manifest must use those same
  verified archive digests.
- Direct `.deb` installation has no Aplexica APT repository signature. Verify
  the downloaded `.deb` with this release procedure before installing it.
- A source checkout uses git-host trust. The source release asset is the source
  artifact covered by the KMS-signed manifest and provenance bundle.

## Anti-rollback

A valid KMS signature remains valid after a newer release exists. The exact
filename check prevents a manifest for one version from satisfying a request
for another, but it cannot tell you which version you should choose. Select the
version deliberately from the [releases page](https://github.com/Aplexica/Aplexica/releases).

`aplexica update` is advisory. It reads release metadata, reports newer version
availability, and maintains a local highest-seen-version floor; it does not
download, authenticate, or install release artifacts. Perform the verification
above before installing anything it reports.

## Report a verification problem

Do not execute a file after a signature, public-key, transparency, provenance,
filename, or digest failure. Preserve the rejected files for analysis and use
Aplexica's [security policy](../../SECURITY.md) to report the evidence.
