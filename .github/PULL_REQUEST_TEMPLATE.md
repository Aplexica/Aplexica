<!--
Thank you for opening a PR! Please skim the checklist below before requesting review.
For non-trivial changes, open or link an issue first so we can align on scope.
-->

## Summary

<!-- One or two sentences. What does this PR do, at a user-visible level? -->

## Motivation

<!-- Why is this change worth making? Link the issue or design discussion if one exists. -->

Closes #

## Changes

<!-- Bulleted list of the substantive things this PR touches. Keep it terse. -->

-
-
-

## Test plan

<!-- How did you verify? What new tests did you add? On which platforms have you run? -->

- [ ] `make test-race` green on …
- [ ] `make magiclint` green
- [ ] `gofmt -l .` empty; `go vet ./...` clean
- [ ] New behaviour has tests
- [ ] CHANGELOG.md updated under the topmost unreleased version section (there
  is no literal `[Unreleased]` section).

## BRD / FR / ADR references

<!-- If your change implements or modifies a documented requirement, link it. -->

- BRD:
- FR:
- ADR:

## Notes for reviewers

<!-- Anything you'd like reviewers to focus on, or anything you're unsure about. -->

---

By submitting this PR I confirm:

- [ ] I have signed the [Contributor License Agreement](../CONTRIBUTING.md#contributor-license-agreement-cla) (the bot comments on your first PR with the signing link).
- [ ] I am not adding LLM summarization, lossy consolidation, or non-deterministic transformation on the canonical-format path (see [docs/00-vision.md](../docs/00-vision.md)).
- [ ] I am not introducing new external network calls from the OSS daemon (see [09-security-and-trust-model.md](../docs/09-security-and-trust-model.md) §3).
