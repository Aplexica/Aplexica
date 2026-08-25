# Canonical Homebrew formula for the Aplexica/homebrew-tap repository.
#
# This file is the source of truth for the tap's Formula/aplexica.rb. It is a
# BINARY formula: it installs the executables from a published release
# archive whose digest is covered by the release's cosign-signed SHA256SUMS.
# It deliberately does NOT build from source, because the release archive
# already carries the compiled-in local web UI.
#
# A source-controlled workflow DOES publish this formula: the `tap` job in
# .github/workflows/release.yml substitutes VERSION_PLACEHOLDER and the four
# SHA256_* placeholders below with digests read out of the release's
# cosign-verified SHA256SUMS, then pushes the result to Aplexica/homebrew-tap.
# That job is gated on the repository variable TAP_PUBLISH_ENABLED, so while
# the gate is off the bump is done by hand (see the "Homebrew tap" section of
# docs/RELEASING.md) — from the same source of digests, under the same rule.
#
# Release authority is the AWS KMS-backed cosign signature over SHA256SUMS,
# verified with the independently distributed `aplexica-release.pub`.
#
# The four sha256 values below MUST therefore be read out of a COSIGN-VERIFIED
# SHA256SUMS — "cosign-verified" and not merely "downloaded", because an
# unverified SHA256SUMS is worthless: anyone able to swap an archive is
# equally able to swap the digest list sitting next to it. Run
# `cosign verify-blob` first, transcribe second. docs/install/verify.md
# carries the exact command.

class Aplexica < Formula
  desc "Cross-agent state portability for AI coding agents"
  homepage "https://aplexica.com"
  # `version` must precede `license` or brew style flags FormulaAudit/ComponentsOrder.
  version "VERSION_PLACEHOLDER"
  license "AGPL-3.0-or-later"

  on_macos do
    on_arm do
      url "https://github.com/Aplexica/Aplexica/releases/download/v#{version}/aplexica-#{version}-darwin-arm64.tar.gz"
      sha256 "SHA256_DARWIN_ARM64"
    end
    on_intel do
      url "https://github.com/Aplexica/Aplexica/releases/download/v#{version}/aplexica-#{version}-darwin-amd64.tar.gz"
      sha256 "SHA256_DARWIN_AMD64"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/Aplexica/Aplexica/releases/download/v#{version}/aplexica-#{version}-linux-arm64.tar.gz"
      sha256 "SHA256_LINUX_ARM64"
    end
    on_intel do
      url "https://github.com/Aplexica/Aplexica/releases/download/v#{version}/aplexica-#{version}-linux-amd64.tar.gz"
      sha256 "SHA256_LINUX_AMD64"
    end
  end

  # The release archive is flat — no top-level directory. Verified against
  # aplexica-1.0.2-darwin-arm64.tar.gz, which contains exactly:
  #   aplexica  aplexica-status  aplexicatray
  #   CHANGELOG.md  LICENSE  LICENSE-EXCEPTIONS.md  README.md  SECURITY.md
  def install
    bin.install "aplexica"
    bin.install "aplexica-status"
    bin.install "aplexicatray"

    doc.install "CHANGELOG.md", "LICENSE", "LICENSE-EXCEPTIONS.md", "README.md", "SECURITY.md"
  end

  # No `service do` block on purpose. `aplexica setup --install` registers the
  # daemon with launchd (macOS) or `systemctl --user` (Linux); adding a
  # `brew services` supervisor would fight that one for the same process.

  def caveats
    <<~EOS
      Aplexica installs three executables:
        aplexica        - CLI + daemon + local web UI server (binds 127.0.0.1 only)
        aplexica-status - status helper the tray spawns, so process monitors can
                          tell the watcher apart from the daemon
        aplexicatray    - system-tray indicator

      Complete setup and start the daemon + tray:
        aplexica setup --yes --install

      Do NOT run `brew services start aplexica`. `aplexica setup --install`
      registers the daemon with launchd (macOS) or `systemctl --user` (Linux);
      a second supervisor would fight it.

      Local web UI: click the tray icon -> Open Aplexica, or run:
        aplexica web open

      Linux tray: GNOME needs the AppIndicator/AppIndicatorSupport shell
      extension for the icon to appear. `aplexica web open` works regardless.

      Aplexica Cloud is a SEPARATE commercial component. No Homebrew, apt,
      winget, or direct-download channel ships aplexica-cloud-plugin. If you
      have one, --cloud is only honored together with --install, and the first
      enrollment also needs its out-of-band trust values:
        aplexica setup --yes --install \\
          --cloud /Library/Aplexica/RemotePlugins/aplexica-cloud/vX.Y.Z/aplexica-cloud-plugin \\
          --cloud-initial-sequence N --cloud-initial-rollback-floor F \\
          --cloud-initial-inventory-sha256 <independently-verified-sha256>
      On macOS the plugin tree must be root-owned and read-only; user-owned
      and Homebrew-prefix paths fail closed by design.

      To uninstall cleanly, unregister the services FIRST:
        aplexica daemon uninstall
        aplexica tray uninstall
        brew uninstall aplexica

      User data lives in ~/.aplexica/ and is NOT removed by brew.
    EOS
  end

  test do
    assert_match "v#{version}", shell_output("#{bin}/aplexica --version")
    system bin/"aplexica", "status"
  end
end
