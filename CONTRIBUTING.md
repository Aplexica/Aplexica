# Contributing to Aplexica

Thank you for your interest in contributing! Aplexica is open-source under the **GNU Affero General Public License v3.0** (see [LICENSE](LICENSE)). We accept code, documentation, bug reports, and ideas from anyone.

This document describes how to get a change accepted.

## TL;DR

1. Open or claim an [issue](https://github.com/Aplexica/Aplexica/issues) describing what you intend to do (for non-trivial changes).
2. Fork the repository, create a topic branch, write your change with tests.
3. Push and open a pull request against `main`.
4. On your first PR, sign the Contributor License Agreement (CLA) — the bot will prompt you (see below).
5. CI must be green; at least one maintainer must approve.

## Reporting bugs

Use the **Bug report** issue template. Include:

- Your platform (macOS / Linux / Windows, version, architecture).
- The install channel you used. See [docs/install/](docs/install/_index.md) for
  which channels are live today; the Homebrew tap is not yet bumped, so most
  installs are apt, a release archive (including the Windows `.zip`), or source.
- The exact version (`aplexica --version`; include `aplexicatray --version` for tray issues).
- A minimal reproduction — the shortest set of steps that demonstrates the bug.
- What you expected, what actually happened, and any relevant log excerpts. Run with `--log-level=debug` if the logs are sparse.

For **security vulnerabilities**, do not file a public issue. See [SECURITY.md](SECURITY.md) for the responsible-disclosure address and process.

## Proposing changes

For small, mechanical fixes (typos, obvious bugs, documentation polish) you can skip the design-first step and open a PR directly.

For anything that touches the canonical format, the adapter API, the daemon's
sync pipeline, or a public CLI surface, please open a **design discussion** or
a tracking issue first. The public BRDs in [docs/](docs/) describe the current
product contracts; significant changes need to update the relevant document.

Aplexica's load-bearing differentiator is **deterministic lossless replication** of agent state. Contributions that introduce LLM summarization, lossy consolidation, or non-deterministic transformation on the canonical-format path will not be accepted — see [docs/00-vision.md](docs/00-vision.md) and the architecture explanation in [README.md](README.md#architecture-in-one-paragraph). If your idea seems to require any of those, please open a discussion first so we can find an alternative shape.

## Contributor License Agreement (CLA)

Before we can merge your contribution, you'll need to sign Aplexica's Contributor License Agreement. This is a one-time step that covers all future PRs across every Aplexica open-source repository (this daemon, the portal, etc.). The CLA Assistant bot will prompt you automatically on your first PR — no email back-and-forth and no forms outside GitHub.

- **Individual contributors** sign the [Individual CLA (ICLA)](CLA.md) by commenting the canonical signing phrase on your PR. The bot records the signature and updates the required check.
- **Corporate contributors** (employees contributing on behalf of an employer) should ask their employer to execute the [Corporate CLA (CCLA)](CCLA.md) instead. The CCLA designates which employees are authorized to contribute on behalf of the company and removes the need for those employees to sign the ICLA individually.

### What the CLA actually says (plain language)

Aplexica operates a commercial open-core business. The daemon, the portal, and the public Work will **always remain available under AGPL-3.0-or-later**. The CLA grants Aplexica the right to **also** distribute your contribution under other licenses — including proprietary or commercial licenses — so that we can offer commercial terms to enterprises that cannot accept the AGPL-3.0 obligations.

Practically, this means:

- You retain copyright in your contribution. The CLA is a **license** from you to Aplexica, not an assignment.
- Your name remains in `git log` and any credits we maintain.
- The public AGPL-3.0 daemon is not going anywhere; the open-source license to the public Work is irrevocable.
- Aplexica can build separate commercial products that include your contribution without negotiating a separate license with you each time.
- If you don't want your contribution dual-licensable under these terms, please don't submit it. Forks and AGPL-3.0-only derivative projects are entirely free to use the public Work without signing the CLA — the CLA only applies to contributions back to the canonical repos.

The Version 1.0 agreement text is linked by the CLA Assistant prompt.

## Branch and commit conventions

- Branch from latest `main`. Use a descriptive topic-branch name: `feat/openclaw-skill-roundtrip`, `fix/conflict-merge-truncation`, `docs/quickstart-clarification`, etc.
- One logical change per pull request. Refactor-only commits are welcome but should not be mixed with feature commits.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):
  - `type(scope): short imperative description`
  - `type`: `feat`, `fix`, `chore`, `docs`, `test`, `refactor`, `perf`, `ci`, `revert`.
  - `scope`: a package/area name (`adapter`, `daemon`, `cli`, `sync`, `retention`, `web`, etc.) or omitted for repo-wide changes.
  - Body: the **why**, not the **how**. The diff already shows the how. Reference any related issue (`Closes #123`).

## Code style

### Go (daemon + CLI + tray)

- `gofmt`-clean. The CI lint job runs `gofmt -l` and fails on any output.
- Run `go vet ./...` locally before pushing.
- Use the magic-number lint: `make magiclint`. New tunables must come from `defaults.toml` (FR-10.6); the allowlist `.magiclint-allow` is a budget, not a free-pass list.
- No CGo unless absolutely necessary. The daemon is a static binary by design — adding CGo means changes to the release pipeline and the install docs. The standing exception is darwin, where `github.com/fsnotify/fsevents` links CoreServices and `fyne.io/systray` links Cocoa, so the macOS artifacts are built with `CGO_ENABLED=1` on a macOS runner while linux and windows stay CGo-free — keep it that way, because a new CGo dependency on those platforms would break the cross-compiled release builds.
- Follow [Effective Go](https://go.dev/doc/effective_go) for naming. Exported identifiers get doc comments; tests are table-driven where it makes sense.
- All new public APIs ship with `_test.go` coverage. The existing test files are the best style reference.
- Concurrent code requires a `-race` test that exercises the concurrency. Running `make test-race` is the way to confirm.

### Markdown

- 80-100 column soft wrap; let the tooling reflow.
- Code blocks always have a language tag.

## Running tests locally

```bash
# Unit + integration, all packages, with race detector
make test-race

# Just one package
go test -race -timeout 240s ./internal/sync/...

# Tray-tagged build + tests
make tray
go test -tags tray ./cmd/aplexicatray/...

# Conformance suite (slow)
go test -timeout 600s ./internal/conformance/...
```

CI runs on macOS, Linux (Ubuntu), and Windows. If your change is platform-specific, please verify locally on as many of those as you have access to; mention which platforms you tested in the PR description.

## Pull request checklist

Before requesting review, please confirm:

- [ ] `make test-race` is green on your local platform.
- [ ] `make magiclint` is green (or the allowlist diff is documented).
- [ ] `gofmt -l .` is empty; `go vet ./...` is clean.
- [ ] New behaviour has tests (unit + integration if applicable).
- [ ] Public CLI / config / canonical-format changes are reflected in the relevant BRD or docs page.
- [ ] `CHANGELOG.md` has a one-line entry per user-visible change, under the topmost `## [X.Y.Z]` section **only while that version is still unreleased** — that is, no `vX.Y.Z` tag exists for it yet. If the topmost section is an already-tagged, dated release, do not add to it: open a new `## [X.Y.Z] - unreleased` section above it for the next version, put your entry there, and say so in the PR description so the maintainer dates that same heading at tag time instead of adding a second one. There is no `[Unreleased]` section — the release job reads the first `## [` heading of any shape and fails unless it names the tag being cut. This matters because the release job publishes only the section naming the new tag, so an entry left in an already-published section never reaches the release notes.
- [ ] CLA signed (or CCLA covers your GitHub username) — the bot status check is green.
- [ ] No secrets, no debugging printlns, no commented-out code.

## Review and merging

- A maintainer will review within 5 business days. If you haven't heard back, comment on the PR or ping `@maintainers`.
- Reviews follow the [conventional comments](https://conventionalcomments.org/) style — labels like `suggestion`, `nitpick`, `issue`, `praise`. Engage with each.
- We use **squash merges** so the `main` history is one commit per PR. Your individual commits get squashed; the squashed message is taken from the PR title and description, so write those carefully.
- CHANGELOG entries land with the merge and are included by maintainers in a
  later release.

## Versioning and releases

Aplexica follows [Semantic Versioning](https://semver.org/), with one
restriction: release tags are exact `vMAJOR.MINOR.PATCH` only. The workflow
trigger glob is `v[0-9]+.[0-9]+.[0-9]+`, so SemVer pre-release and build
suffixes such as `v1.1.0-rc.1` are a silent no-op — no run starts and no error
is reported. Add a concise `CHANGELOG.md` entry with each user-visible change.
Merging to `main` does not itself create or publish a release.

A release is cut by pushing an annotated `vX.Y.Z` tag, the only trigger for [.github/workflows/release.yml](.github/workflows/release.yml). The workflow builds every artifact, asks AWS KMS to sign the checksum manifest and public provenance, publishes one GitHub Release, and verifies it using the reviewed public key. Signing and publication run in separate jobs, and there is no manual dispatch path. A published or signed tag is immutable; corrections use a higher version. See [docs/RELEASING.md](docs/RELEASING.md).

## Code of Conduct

This project follows the [Contributor Covenant v2.1](CODE_OF_CONDUCT.md). By participating you agree to abide by it. Report violations privately to `conduct@aplexica.com`.

## Where things live

| Topic | Where |
|---|---|
| Vision and scope | [docs/00-vision.md](docs/00-vision.md) |
| BRDs (capability requirements) | [docs/01-*.md](docs/) through [docs/05-*.md](docs/) |
| Security & trust model | [docs/09-security-and-trust-model.md](docs/09-security-and-trust-model.md) |
| Non-functional requirements | [docs/10-non-functional-requirements.md](docs/10-non-functional-requirements.md) |
| Per-agent adapter specs | [docs/adapters/](docs/adapters/) |
| Per-platform install | [docs/install/](docs/install/) |
| User guide | [docs/user-guide.md](docs/user-guide.md) |
| Plugin protocol | [docs/plugin-protocol-spec.md](docs/plugin-protocol-spec.md) |
| Source: CLI + daemon entry points | [cmd/aplexica/](cmd/aplexica/), [cmd/aplexicatray/](cmd/aplexicatray/) |
| Source: internal packages | [internal/](internal/) |

## Getting help

- General questions, design discussion → [GitHub Discussions](https://github.com/Aplexica/Aplexica/discussions).
- Bug reports → [GitHub Issues](https://github.com/Aplexica/Aplexica/issues).
- Security → [SECURITY.md](SECURITY.md).
- Anything else → `hello@aplexica.com`.

Welcome aboard, and thank you for helping move agent state out of vendor silos.
