# Install the Debian/Ubuntu package

Published Aplexica releases include `.deb` assets for Debian and Ubuntu on
`amd64` and `arm64`. Choose an exact release version from the
[Aplexica releases page](https://github.com/Aplexica/Aplexica/releases); do not
derive the version from an unsigned “latest” redirect.

## Download the package

Replace `X.Y.Z` with the version you selected:

```bash
VERSION=X.Y.Z
ARCH=$(dpkg --print-architecture)
curl -fLO "https://github.com/Aplexica/Aplexica/releases/download/v${VERSION}/aplexica_${VERSION}_${ARCH}.deb"
```

If `dpkg --print-architecture` returns anything other than `amd64` or `arm64`,
use the [source-build instructions](build.md) instead.

**Do not install it yet.** Authenticate the download first — the next section
is the step that decides whether these bytes are a release Aplexica published.

## Verify the package

`apt` validates the structure of a standalone `.deb`, but it does not
authenticate the publisher. The release's `SHA256SUMS` does: it lists the
`.deb` alongside every other published artifact, and its cosign signature is
authorized by the release AWS KMS key.

Download the manifest and its signature next to the package you fetched:

```bash
BASE="https://github.com/Aplexica/Aplexica/releases/download/v${VERSION}"
curl -fLO "$BASE/SHA256SUMS"
curl -fLO "$BASE/SHA256SUMS.sigstore.json"
curl -fLO "$BASE/aplexica.provenance.sigstore.json"
```

Run the complete KMS signature, exact-name, provenance, and policy procedure
from [Verify a release](verify.md), selecting the `.deb`, and only then install
it.

```bash
sha256sum --check --ignore-missing SHA256SUMS
```

`--ignore-missing` limits the check to the files actually present in the
current directory, so it reports on your `.deb` alone. Do not install a `.deb`
whose digest does not match, and do not treat an unsigned checksum served
beside the package as independent authentication.

## Install the verified package

```bash
sudo apt install "./aplexica_${VERSION}_${ARCH}.deb"
```

Complete setup as your normal user:

```bash
aplexica setup --yes --install
aplexica status
```

The package installs `aplexica`, `aplexica-status`, and `aplexicatray`, plus
per-user systemd unit definitions. Setup enables them for the current account
so Aplexica state is written with the correct ownership.

There is no official Aplexica APT repository at public launch. Consequently,
package-name-only installation is not supported; install an exact local `.deb`
as shown above. Do not add a third-party repository claiming to provide
Aplexica.

## Uninstall

Unregister the per-user services before removing the package:

```bash
aplexica daemon uninstall
aplexica tray uninstall
sudo apt remove aplexica
```

The package removal leaves user data under `~/.aplexica/` in place. Back up or
archive that directory before deciding whether to discard any data. See the
[general uninstall guidance](_index.md#uninstalling).
