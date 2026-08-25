#!/usr/bin/env bash
# shellcheck disable=SC1003,SC2016 # Literal backslashes and Makefile/YAML source assertions.
#
# Release-path security gate. This runs on the ubuntu, macOS and Windows legs
# of the Test workflow with no `if:` guard, so whatever it asserts here is
# asserted on every push, on every platform, before anything can be tagged.
#
# It asserts the complete AWS KMS release path and rejects any unsupported
# second authority. A weakened release path can otherwise remain invisible
# until a user tries to verify bytes that have already been published.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RELEASE_WORKFLOW="$ROOT/.github/workflows/release.yml"
VERIFY_DOC="$ROOT/docs/install/verify.md"
RELEASING_DOC="$ROOT/docs/RELEASING.md"
GORELEASER="$ROOT/.goreleaser.yaml"
HOMEBREW_FORMULA="$ROOT/packaging/homebrew/aplexica.rb"
WINGET_INSTALLER="$ROOT/packaging/winget/Aplexica.Aplexica.installer.yaml"
PUBLIC_KEY="$ROOT/aplexica-release.pub"

fail() { printf 'installer security test: %s\n' "$*" >&2; exit 1; }
require_fixed() { grep -Fq -- "$2" "$1" || fail "$1 is missing required text: $2"; }
reject_regex() { if grep -Eiq -- "$2" "$1"; then fail "$1 $3"; fi; }
first_line() {
  local line
  line="$(grep -nF -- "$2" "$1" | awk -F: 'NR == 1 { print $1 }')"
  [ -n "$line" ] || fail "$1 is missing ordering marker: $2"
  printf '%s\n' "$line"
}
assert_before() {
  local earlier later
  earlier="$(first_line "$1" "$2")"; later="$(first_line "$1" "$3")"
  [ "$earlier" -lt "$later" ] || fail "$1 must perform '$2' before '$3'"
}
require_exact_line() {
  grep -Fqx -- "$2" "$1" || fail "$1 is missing required exact line: $2"
}
extract_yaml_job() {
  local source job destination matches
  source="$1"
  job="$2"
  destination="$3"
  matches="$(awk -v job="$job" '
    {
      line = $0
      sub(/\r$/, "", line)
      if (line == "  " job ":") count++
    }
    END { print count + 0 }
  ' "$source")"
  [ "$matches" -eq 1 ] \
    || fail "$source must define exactly one '$job' job; found $matches"
  awk -v job="$job" '
    {
      line = $0
      sub(/\r$/, "", line)
    }
    line == "  " job ":" { found = 1 }
    found && line != "  " job ":" && line ~ /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { exit }
    found { print }
  ' "$source" > "$destination"
}
extract_yaml_top_level() {
  local source key destination matches
  source="$1"
  key="$2"
  destination="$3"
  matches="$(awk -v key="$key" '
    {
      line = $0
      sub(/\r$/, "", line)
      if (line == key ":") count++
    }
    END { print count + 0 }
  ' "$source")"
  [ "$matches" -eq 1 ] \
    || fail "$source must define exactly one top-level '$key' block; found $matches"
  awk -v key="$key" '
    {
      line = $0
      sub(/\r$/, "", line)
    }
    line == key ":" { found = 1 }
    found && line != key ":" && line ~ /^[A-Za-z0-9_-]+:[[:space:]]*$/ { exit }
    found { print }
  ' "$source" > "$destination"
}
extract_yaml_step() {
  local source marker destination matches
  source="$1"
  marker="$2"
  destination="$3"
  matches="$(grep -Fc -- "$marker" "$source" || true)"
  [ "$matches" -eq 1 ] \
    || fail "$source must contain '$marker' in exactly one step; found $matches occurrences"
  awk -v marker="$marker" '
    /^      -[[:space:]]+/ {
      if (wanted) {
        printf "%s", block
        emitted = 1
        exit
      }
      block = ""
      wanted = 0
    }
    {
      block = block $0 "\n"
      if (index($0, marker)) wanted = 1
    }
    END { if (wanted && !emitted) printf "%s", block }
  ' "$source" > "$destination"
  [ -s "$destination" ] || fail "$source has no YAML step containing '$marker'"
}
extract_continued_command() {
  local source marker
  source="$1"
  marker="$2"
  awk -v marker="$marker" '
    index($0, marker) { copying = 1 }
    copying {
      line = $0
      sub(/\r$/, "", line)
      sub(/^[[:space:]]*/, "", line)
      print line
      if (line !~ /\\[[:space:]]*$/) exit
    }
  ' "$source"
}
extract_bash_fence() {
  local source marker destination matches
  source="$1"
  marker="$2"
  destination="$3"
  matches="$(awk -v marker="$marker" '
    /^```bash[[:space:]]*\r?$/ { inside = 1; wanted = 0; next }
    inside && /^```[[:space:]]*\r?$/ {
      if (wanted) count++
      inside = 0
      next
    }
    inside && index($0, marker) { wanted = 1 }
    END { print count + 0 }
  ' "$source")"
  [ "$matches" -eq 1 ] \
    || fail "$source must contain exactly one bash block with '$marker'; found $matches"
  awk -v marker="$marker" '
    /^```bash[[:space:]]*\r?$/ { inside = 1; wanted = 0; block = ""; next }
    inside && /^```[[:space:]]*\r?$/ {
      if (wanted) {
        printf "%s", block
        exit
      }
      inside = 0
      next
    }
    inside {
      block = block $0 "\n"
      if (index($0, marker)) wanted = 1
    }
  ' "$source" > "$destination"
  [ -s "$destination" ] || fail "$source has no bash block containing '$marker'"
}
extract_heredoc() {
  local source marker terminator destination matches
  source="$1"
  marker="$2"
  terminator="$3"
  destination="$4"
  matches="$(grep -Fc -- "$marker" "$source" || true)"
  [ "$matches" -eq 1 ] \
    || fail "$source must contain exactly one heredoc starting with '$marker'; found $matches"
  awk -v marker="$marker" -v terminator="$terminator" '
    !copying && index($0, marker) {
      indent = match($0, /[^[:space:]]/) - 1
      copying = 1
    }
    copying {
      line = substr($0, indent + 1)
      print line
      if (line == terminator) {
        found_end = 1
        exit
      }
    }
    END { if (!found_end) exit 2 }
  ' "$source" > "$destination" \
    || fail "$source has an unterminated '$marker' heredoc"
}

# Every other packaging script parses. These two are /bin/sh and are checked
# for syntax only, not dialect — bash -n accepts the POSIX subset they are
# written in — but a dpkg maintainer script that fails to parse breaks the
# install after the package is already unpacked, on the user's machine, with no
# earlier signal. This file needs no such check: bash could not have reached
# this line without parsing all of it.
bash -n "$ROOT/packaging/nfpm/deb/postinst.sh"
bash -n "$ROOT/packaging/nfpm/deb/prerm.sh"

# The daemon package ldflag alone does not update main.trayVersion, so a tray
# built without this one stamps whatever the source default happens to be and
# reports a version the release never shipped.
require_fixed "$ROOT/Makefile" 'main.trayVersion=$(GIT_VERSION)'

# ---------------------------------------------------------------------------
# The release workflow exists at the canonical automation path.
#
# This is the exact inverse of the assertion this file used to carry. Keeping a
# single fixed entry point lets the policy checks, IAM trust policy, runbook and
# release operator all name the same workflow rather than leaving an unnoticed
# second publication path.
[ -e "$RELEASE_WORKFLOW" ] || fail 'release workflow must exist at .github/workflows/release.yml'
[ -f "$PUBLIC_KEY" ] || fail 'aplexica-release.pub is missing; no KMS-signed release can ship without its independently reviewed public trust anchor'
( cd "$ROOT" && go run -mod=readonly ./tools/releasekeycheck --public-key "$PUBLIC_KEY" >/dev/null ) \
  || fail 'aplexica-release.pub is not the required ECC_NIST_P256 PKIX public key'

# Presence and ordering are asserted against a COMMENT-STRIPPED copy, never the
# file as written. This workflow is deliberately comment-dense — the signing
# step alone carries nine lines of rationale — and a plain substring grep cannot
# tell executable YAML from prose about it. Without this, deleting the whole
# `Sign SHA256SUMS` step and leaving behind a note saying it used to run
# `cosign sign-blob` into `SHA256SUMS.sigstore.json` satisfies every check
# below, including the ordering one, and the gate reports a signed release that
# does not exist. Rejections stay on the real file: a forbidden string in a
# comment is worth failing on, and matching a superset never lets one through.
#
# Line numbering is preserved (sed rewrites lines, it does not drop them), so
# the ordering assertion still reports positions a reader can find in the file.
# The copy is given a self-describing basename so a failure message points a
# reader at what was actually searched instead of a bare mktemp path.
WF_CODE_DIR="$(mktemp -d)"
trap 'rm -rf "$WF_CODE_DIR"' EXIT
WF_CODE="$WF_CODE_DIR/release.yml.comments-stripped"
awk '
  /^[[:space:]]*#/ { print ""; next }
  {
    line = $0
    if (line ~ /^[[:space:]]*(-[[:space:]]+)?uses:/) {
      sub(/[[:space:]]+#.*$/, "", line)
    }
    print line
  }
' "$RELEASE_WORKFLOW" > "$WF_CODE"

# Job-local copies prevent one authority from impersonating another.
GUARD_JOB="$WF_CODE_DIR/guard.job.yml"
BUILD_JOB="$WF_CODE_DIR/build.job.yml"
SIGN_JOB="$WF_CODE_DIR/sign.job.yml"
PUBLISH_JOB="$WF_CODE_DIR/publish.job.yml"
VERIFY_JOB="$WF_CODE_DIR/verify.job.yml"
TAP_JOB="$WF_CODE_DIR/tap.job.yml"
extract_yaml_job "$WF_CODE" guard "$GUARD_JOB"
extract_yaml_job "$WF_CODE" build "$BUILD_JOB"
extract_yaml_job "$WF_CODE" sign "$SIGN_JOB"
extract_yaml_job "$WF_CODE" publish "$PUBLISH_JOB"
extract_yaml_job "$WF_CODE" verify "$VERIFY_JOB"
extract_yaml_job "$WF_CODE" tap "$TAP_JOB"

# No seventh job may sit outside the six job-local authority checks below.
# A contents-read job can still publish with an unreviewed PAT, and neither the
# named-job digests nor workflowpolicy can infer that arbitrary secret's scope.
job_keys="$(awk '
  /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
  in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
    key = $0
    sub(/^  /, "", key)
    sub(/:.*/, "", key)
    print key
  }
' "$WF_CODE")"
expected_job_keys="$(printf '%s\n' guard build sign publish verify tap)"
[ "$job_keys" = "$expected_job_keys" ] \
  || fail "release workflow job set or order drifted; found:\n$job_keys"

# Permission maps are allow-lists, not merely checks for the two powers we
# happen to expect. An added `packages: write`, `actions: write`, or similar
# scope on the signing job is a second publication authority even if contents
# remains read-only.
ROOT_PERMISSIONS="$WF_CODE_DIR/root.permissions.yml"
extract_yaml_top_level "$WF_CODE" permissions "$ROOT_PERMISSIONS"
root_permissions="$(awk 'NF { line = $0; sub(/^[[:space:]]*/, "", line); sub(/[[:space:]]+$/, "", line); print line }' "$ROOT_PERMISSIONS")"
[ "$root_permissions" = "$(printf '%s\n' 'permissions:' 'contents: read')" ] \
  || fail "release workflow root permissions drifted; found:\n$root_permissions"
assert_job_permissions() {
  local job expected actual
  job="$1"
  expected="$2"
  actual="$(awk '
    /^    permissions:[[:space:]]*$/ { copying = 1 }
    copying && /^    [A-Za-z0-9_-]+:/ && $0 !~ /^    permissions:/ { exit }
    copying && NF {
      line = $0
      sub(/^[[:space:]]*/, "", line)
      sub(/[[:space:]]+$/, "", line)
      print line
    }
  ' "$job")"
  [ "$actual" = "$expected" ] \
    || fail "$job permissions drifted; found:\n$actual"
}
read_permissions="$(printf '%s\n' 'permissions:' 'contents: read')"
assert_job_permissions "$GUARD_JOB" "$read_permissions"
assert_job_permissions "$BUILD_JOB" "$read_permissions"
assert_job_permissions "$SIGN_JOB" "$(printf '%s\n' 'permissions:' 'contents: read' 'id-token: write')"
assert_job_permissions "$PUBLISH_JOB" "$(printf '%s\n' 'permissions:' 'contents: write')"
assert_job_permissions "$VERIFY_JOB" "$read_permissions"
assert_job_permissions "$TAP_JOB" "$read_permissions"

# Pin every executable line of every release job after comments and blank lines
# are removed. The focused assertions below provide useful diagnostics; these
# digests are the completeness backstop. They make fail-open edits such as
# `|| true`, replacing `exit 1` with `:`, indirect signer variables, extra API
# calls, or an additional privileged step mechanically visible even when no
# regex anticipated the spelling.
sha256_file() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | awk '{ print $NF }'
  else
    fail 'no SHA-256 implementation is available to pin the release job programs'
  fi
}
assert_job_program() {
  local name job expected normalized actual
  name="$1"
  job="$2"
  expected="$3"
  normalized="$WF_CODE_DIR/$name.job-program.yml"
  awk 'NF { sub(/[[:space:]]+$/, ""); print }' "$job" > "$normalized"
  actual="$(sha256_file "$normalized")"
  [ "$actual" = "$expected" ] \
    || fail "$name job executable program drifted (expected $expected, found $actual)"
}
assert_job_program guard "$GUARD_JOB" '212cb4184839ace346cb090b4655d3f6f203d1bb2563b4318543fc2188434590'
assert_job_program build "$BUILD_JOB" '6c88e2be52df2efba54021ba54beb6ed4e723efb7a9e56118e43b923eec058a0'
assert_job_program sign "$SIGN_JOB" '94b599426c2f79d9a966ceb42090f7ce594e9d64e8032c8f3a217fb97facdf7c'
assert_job_program publish "$PUBLISH_JOB" 'd925d21fa79e9c5eafb04d2ba56f1e5c45c4a7ea6b46e612a8323af8a5c74235'
assert_job_program verify "$VERIFY_JOB" 'b97537eacbad383d73bbe1bbca5316d1219fe2bd77c1bca848b4e402354153c4'
assert_job_program tap "$TAP_JOB" '8e699118a051341653f951bcd431badb9ec3a4af4714bd38e8f94f0259c63484'

# The same completeness backstop applies to the release compiler/packager.
# Focused archive and naming checks below explain individual invariants; this
# digest also binds every build entry point, tag, ldflag, trimpath/buildid flag,
# package script, checksum input, source prefix, and disabled publisher.
GORELEASER_PROGRAM="$WF_CODE_DIR/goreleaser.executable.yml"
sed 's/#.*$//' "$GORELEASER" \
  | awk 'NF { sub(/[[:space:]]+$/, ""); print }' > "$GORELEASER_PROGRAM"
goreleaser_program_digest="$(sha256_file "$GORELEASER_PROGRAM")"
[ "$goreleaser_program_digest" = 'c2112989bba52edf350f1357bdc86e838d13fd80449243c9a6aa65b711b55c6e' ] \
  || fail "GoReleaser executable configuration drifted (found $goreleaser_program_digest)"

# A release tag must not publish a changelog section that is still marked
# Unreleased or carries an impossible calendar date.
CHANGELOG_STEP="$WF_CODE_DIR/changelog.step.yml"
extract_yaml_step "$GUARD_JOB" 'heading=$(awk' "$CHANGELOG_STEP"
require_fixed "$CHANGELOG_STEP" 'version=${GITHUB_REF_NAME:1}'
require_fixed "$CHANGELOG_STEP" 'heading=$(awk '\''$1 == sprintf("%c%c", 35, 35) && substr($2, 1, 1) == "[" { print; exit }'\'' CHANGELOG.md)'
require_fixed "$CHANGELOG_STEP" 'heading_fields=$(printf '\''%s\n'\'' "$heading" | awk '\''{ print NF }'\'')'
require_fixed "$CHANGELOG_STEP" 'heading_separator=$(printf '\''%s\n'\'' "$heading" | awk '\''{ print $3 }'\'')'
require_fixed "$CHANGELOG_STEP" 'release_date=$(printf '\''%s\n'\'' "$heading" | awk '\''{ print $4 }'\'')'
require_fixed "$CHANGELOG_STEP" '[[ ! "$release_date" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]'
require_fixed "$CHANGELOG_STEP" 'parsed_date=$(date -u -d "$release_date" +%F 2>/dev/null || true)'
require_fixed "$CHANGELOG_STEP" 'if [ "$parsed_date" != "$release_date" ]; then'

require_exact_line "$BUILD_JOB" '    needs: guard'
require_exact_line "$SIGN_JOB" '    needs: build'
require_exact_line "$PUBLISH_JOB" '    needs: sign'
require_exact_line "$VERIFY_JOB" '    needs: publish'
require_exact_line "$TAP_JOB" '    needs: verify'

portal_stage_calls="$(grep -Fc -- 'run: make fetch-portal' "$BUILD_JOB" || true)"
portal_test_calls="$(grep -Fc -- 'run: go test -tags release ./internal/web/embed/' "$BUILD_JOB" || true)"
[ "$portal_stage_calls" -eq 1 ] && [ "$portal_test_calls" -eq 1 ] \
  || fail "build must stage and test the public Portal exactly once; found stage=$portal_stage_calls test=$portal_test_calls"
require_exact_line "$BUILD_JOB" '        run: make fetch-portal PORTAL_ASSET_SOURCE="$APLEXICA_CI_HANDOFF/portal/v0.1.12"'
require_exact_line "$ROOT/packaging/portal-release.json" '{"repository":"Aplexica/aplexica-portal","tag":"v0.1.12","asset":"aplexica-portal-v0.1.12-local.tar.gz","sha256":"255ea1ff3fbaf7e570e64dbd9c73d36a7134dae85ca2fe225a94dc50816e4db3"}'
assert_before "$BUILD_JOB" 'run: make fetch-portal' 'run: go test -tags release ./internal/web/embed/'
require_fixed "$ROOT/Makefile" 'fetch-portal-git'
require_fixed "$ROOT/packaging/scripts/fetch-portal-git.sh" 'PORTAL_FETCH_TOKEN'
require_fixed "$ROOT/packaging/scripts/fetch-portal-git.sh" 'fetch --depth=1'
require_fixed "$BUILD_JOB" 'rm -rf "$RUNNER_TEMP"/portal-*'
assert_before "$BUILD_JOB" 'rm -rf "$RUNNER_TEMP"/portal-*' 'run: make fetch-portal'
require_fixed "$BUILD_JOB" './packaging/scripts/pack-source-archive.sh'
require_fixed "$BUILD_JOB" '/opt/homebrew/bin/gtar'
require_fixed "$BUILD_JOB" '"$brew_bin" install gnu-tar'
require_fixed "$ROOT/packaging/scripts/pack-source-archive.sh" '--sort=name --format=gnu'
assert_before "$BUILD_JOB" 'run: go test -tags release ./internal/web/embed/' 'uses: goreleaser/goreleaser-action@'

require_fixed "$GUARD_JOB" 'git fetch --no-tags origin main:refs/remotes/origin/main'
require_fixed "$GUARD_JOB" 'git merge-base --is-ancestor "$tag_commit" refs/remotes/origin/main'
for release_gate in \
  'go mod tidy -diff' \
  'go mod verify' \
  'go test -race -count=1 ./...' \
  'go vet ./...' \
  'go run -mod=readonly ./tools/magiclint' \
  'go run -mod=readonly ./tools/actionpin' \
  'go run -mod=readonly ./tools/workflowpolicy' \
  './packaging/scripts/test-installer-security.sh'; do
  require_fixed "$GUARD_JOB" "$release_gate"
done
GUARD_POLICY_STEP="$WF_CODE_DIR/guard.release-policy-step.yml"
GUARD_POLICY_STEP_NORMALIZED="$WF_CODE_DIR/guard.release-policy-step.normalized.yml"
EXPECTED_GUARD_POLICY_STEP="$WF_CODE_DIR/guard.release-policy-step.expected.yml"
extract_yaml_step "$GUARD_JOB" 'name: Run the release policy and test suite' "$GUARD_POLICY_STEP"
awk 'NF { line = $0; sub(/^[[:space:]]*/, "", line); sub(/[[:space:]]+$/, "", line); print line }' \
  "$GUARD_POLICY_STEP" > "$GUARD_POLICY_STEP_NORMALIZED"
cat > "$EXPECTED_GUARD_POLICY_STEP" <<'GUARD_POLICY_STEP'
- name: Run the release policy and test suite
shell: bash
run: |
set -euo pipefail
go mod tidy -diff
go mod verify
go test -race -count=1 ./...
go vet ./...
go run -mod=readonly ./tools/magiclint
go run -mod=readonly ./tools/actionpin
go run -mod=readonly ./tools/workflowpolicy
./packaging/scripts/test-installer-security.sh
GUARD_POLICY_STEP
cmp -s "$GUARD_POLICY_STEP_NORMALIZED" "$EXPECTED_GUARD_POLICY_STEP" \
  || fail "release guard policy/test step drifted or suppresses a failure:\n$(cat "$GUARD_POLICY_STEP_NORMALIZED")"

require_fixed "$SIGN_JOB" 'id-token: write'
require_fixed "$SIGN_JOB" 'cosign sign-blob'
require_fixed "$SIGN_JOB" 'cosign attest-blob'
require_fixed "$SIGN_JOB" 'go run -mod=readonly ./tools/releaseprovenance'
require_fixed "$SIGN_JOB" 'aws-actions/configure-aws-credentials@e6de054238d6b7531b4efff3b6587d9aade6a06c'
require_fixed "$PUBLISH_JOB" 'contents: write'
require_fixed "$PUBLISH_JOB" '${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/releases'
require_fixed "$PUBLISH_JOB" 'https://uploads.github.com/repos/${GITHUB_REPOSITORY}/releases/${release_id}/assets?name=${name}'
require_fixed "$PUBLISH_JOB" 'go run -mod=readonly ./tools/releaseprovenance'
require_fixed "$VERIFY_JOB" 'cosign verify-blob'
require_fixed "$VERIFY_JOB" 'go run -mod=readonly ./tools/releaseprovenance'

for verifier in "$SIGN_JOB" "$PUBLISH_JOB" "$VERIFY_JOB"; do
  count="$(grep -Fc -- '--verify-bundle' "$verifier" || true)"
  [ "$count" -eq 1 ] \
    || fail "$verifier must enforce provenance semantics exactly once; found $count policy verifications"
done
sign_commit_bindings="$(grep -Fc -- '--commit "$(git rev-parse HEAD)"' "$SIGN_JOB" || true)"
publish_commit_bindings="$(grep -Fc -- '--commit "$(git rev-parse HEAD)"' "$PUBLISH_JOB" || true)"
verify_commit_bindings="$(grep -Fc -- '--commit "$(git rev-parse HEAD)"' "$VERIFY_JOB" || true)"
[ "$sign_commit_bindings" -eq 2 ] && [ "$publish_commit_bindings" -eq 1 ] && [ "$verify_commit_bindings" -eq 1 ] \
  || fail "provenance generation and every policy verification must bind the checked-out commit; found sign=$sign_commit_bindings publish=$publish_commit_bindings verify=$verify_commit_bindings"

# Signing authority and publication authority are deliberately disjoint. A
# compromised build must not obtain either; a signing job cannot publish; and
# a publisher cannot mint another signature.
id_token_writers="$(grep -Fc -- 'id-token: write' "$WF_CODE" || true)"
[ "$id_token_writers" -eq 1 ] || fail "release workflow must grant id-token: write to exactly the sign job; found $id_token_writers"
contents_writers="$(grep -Fc -- 'contents: write' "$WF_CODE" || true)"
[ "$contents_writers" -eq 1 ] || fail "release workflow must grant contents: write to exactly the publish job; found $contents_writers"
if grep -Fq -- 'contents: write' "$SIGN_JOB"; then
  fail 'sign job must not have contents: write'
fi
if grep -Fq -- 'id-token: write' "$PUBLISH_JOB"; then
  fail 'publish job must not have id-token: write'
fi
for unprivileged in "$BUILD_JOB" "$VERIFY_JOB" "$TAP_JOB"; do
  if grep -Eq 'id-token: write|contents: write' "$unprivileged"; then
    fail "$unprivileged must have neither signing nor publication authority"
  fi
done

# Permissions alone do not confine authority when a job can name an alternate
# credential. A PAT in the sign job can publish despite `contents: read`, and
# static AWS credentials in the build job can sign despite having no OIDC
# token. Bind the secret interface as tightly as the permissions interface:
# the build receives no repository secret, and the sign job receives exactly
# the four reviewed AWS inputs. The short-lived credential outputs are step
# outputs, not `secrets.*`, so they are deliberately outside this list.
assert_secret_interface() {
  local job expected actual
  job="$1"
  expected="$2"
  actual="$(grep -oE 'secrets\.[A-Za-z0-9_]+' "$job" | sort -u || true)"
  [ "$actual" = "$expected" ] \
    || fail "$job secret interface drifted; found:
$actual"
}
assert_secret_interface "$GUARD_JOB" 'secrets.HOMEBREW_TAP_TOKEN'
assert_secret_interface "$BUILD_JOB" ''
[ "$(grep -Fc 'Install envsubst for the cosign installer' "$WF_CODE")" -eq 4 ] || fail 'envsubst must precede every cosign-installer'
[ "$(grep -Fc 'sudo apt-get install -y gettext-base' "$WF_CODE")" -eq 4 ] || fail 'gettext-base must be installed before every cosign-installer'
assert_secret_interface "$SIGN_JOB" "$(printf '%s\n' \
  'secrets.AWS_RELEASE_ACCOUNT_ID' \
  'secrets.AWS_RELEASE_KMS_KEY_URI' \
  'secrets.AWS_RELEASE_SIGNING_REGION' \
  'secrets.AWS_RELEASE_SIGNING_ROLE_ARN')"
assert_secret_interface "$PUBLISH_JOB" 'secrets.GITHUB_TOKEN'
assert_secret_interface "$VERIFY_JOB" ''
assert_secret_interface "$TAP_JOB" "$(printf '%s\n' \
  'secrets.GITHUB_TOKEN' \
  'secrets.HOMEBREW_TAP_TOKEN')"
if grep -Fq -- 'secrets[' "$WF_CODE"; then
  fail 'release workflow must use only literal dot-form secret names; bracket or computed secret lookup is forbidden'
fi
if grep -Eq -- 'secrets[[:space:]]*\.\*|(^|[^A-Za-z0-9_.])secrets[[:space:]]*([})]|$)|(^|[^A-Za-z0-9_])(join|toJSON|fromJSON)\([[:space:]]*secrets([[:space:]]*[,.])' "$WF_CODE"; then
  fail 'release workflow must not expose the whole secrets object or use a wildcard/object-filter secret lookup'
fi

# The product key authorizes exactly two operations: one message signature and
# one DSSE attestation. Counting over executable YAML, rather than merely
# extracting the first matching command from the sign step, makes a third
# bundle or an unreviewed signer in another job fail. Direct `aws kms sign` is
# forbidden too; all signing must pass through the two reviewed cosign calls.
sign_blob_calls="$(grep -Ec -- '(^|[/;&|[:space:]])cosign[[:space:]]+sign-blob([;&|[:space:]]|$)' "$WF_CODE" || true)"
attest_blob_calls="$(grep -Ec -- '(^|[/;&|[:space:]])cosign[[:space:]]+attest-blob([;&|[:space:]]|$)' "$WF_CODE" || true)"
[ "$sign_blob_calls" -eq 1 ] \
  || fail "release workflow must execute exactly one cosign sign-blob operation; found $sign_blob_calls"
[ "$attest_blob_calls" -eq 1 ] \
  || fail "release workflow must execute exactly one cosign attest-blob operation; found $attest_blob_calls"
aws_credential_actions="$(grep -Fc -- 'aws-actions/configure-aws-credentials@' "$WF_CODE" || true)"
[ "$aws_credential_actions" -eq 1 ] \
  || fail "release workflow must configure AWS credentials exactly once in the sign job; found $aws_credential_actions"
if grep -Eq -- '^[[:space:]]*(run:[[:space:]]*)?((/usr(/local)?/bin/)?aws)([;&|[:space:]\\]|$)|[;&|][[:space:]]*((/usr(/local)?/bin/)?aws)([;&|[:space:]\\]|$)' "$WF_CODE"; then
  fail 'release workflow must not invoke the AWS CLI; signing is limited to the two reviewed cosign operations'
fi
for unsigned_job in "$BUILD_JOB" "$PUBLISH_JOB" "$VERIFY_JOB" "$TAP_JOB"; do
  if grep -Eq 'cosign[[:space:]]+(sign-blob|attest-blob)|aws[[:space:]]+kms[[:space:]]+sign' "$unsigned_job"; then
    fail "$unsigned_job contains signing outside the sign job"
  fi
  if grep -Eq '^[[:space:]]*(AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|AWS_SHARED_CREDENTIALS_FILE|AWS_PROFILE|AWS_REGION|AWS_DEFAULT_REGION):' "$unsigned_job"; then
    fail "$unsigned_job injects static or ambient AWS credentials outside the sign job"
  fi
done

# A contents-read GITHUB_TOKEN is not publication authority, but a separately
# scoped PAT is. Publication uses reviewed curl against the Releases API; forbid
# gh release mutations and GitHub credential injection into the signing boundary.
release_mutations_all="$(grep -Ec -- 'gh release (create|upload|edit|delete)' "$WF_CODE" || true)"
[ "$release_mutations_all" -eq 0 ] \
  || fail "release workflow must contain zero gh release mutations; found $release_mutations_all"
if grep -Eq '(^|[[:space:]])(GH_TOKEN|GITHUB_TOKEN):|secrets\.(GH_TOKEN|GITHUB_TOKEN)' "$SIGN_JOB"; then
  fail 'sign job must not receive a GitHub publication credential'
fi
if grep -Eq -- 'gh[[:space:]]+api' "$SIGN_JOB"; then
  fail 'sign job must not call the GitHub API'
fi

# Assert the complete signing command, not just its verb and output filename.
# Otherwise changing the final operand signs an unrelated artifact while the
# release still publishes a plausibly named SHA256SUMS bundle.
SIGN_STEP="$WF_CODE_DIR/sign.cosign-step.yml"
extract_yaml_step "$SIGN_JOB" 'cosign sign-blob' "$SIGN_STEP"

# Pin the two places where AWS authority is materialized. A verb counter cannot
# see `signer=cosign; "$signer" sign-blob`, and a second step can reuse action
# outputs unless those outputs occur only in this exact reviewed step. Exact
# executable-step comparisons make the two-operation claim mechanical.
AWS_STEP="$WF_CODE_DIR/sign.aws-credentials-step.yml"
extract_yaml_step "$SIGN_JOB" 'aws-actions/configure-aws-credentials@e6de054238d6b7531b4efff3b6587d9aade6a06c' "$AWS_STEP"
AWS_STEP_NORMALIZED="$WF_CODE_DIR/sign.aws-credentials-step.normalized.yml"
EXPECTED_AWS_STEP="$WF_CODE_DIR/sign.aws-credentials-step.expected.yml"
awk 'NF { line = $0; sub(/^[[:space:]]*/, "", line); sub(/[[:space:]]+$/, "", line); print line }' \
  "$AWS_STEP" > "$AWS_STEP_NORMALIZED"
cat > "$EXPECTED_AWS_STEP" <<'AWS_STEP'
- name: Obtain one short-lived AWS signing session
id: aws
uses: aws-actions/configure-aws-credentials@e6de054238d6b7531b4efff3b6587d9aade6a06c
with:
role-to-assume: ${{ secrets.AWS_RELEASE_SIGNING_ROLE_ARN }}
aws-region: ${{ secrets.AWS_RELEASE_SIGNING_REGION }}
audience: sts.amazonaws.com
allowed-account-ids: ${{ secrets.AWS_RELEASE_ACCOUNT_ID }}
mask-aws-account-id: true
role-duration-seconds: 900
role-session-name: aplexica-release-${{ github.run_id }}
unset-current-credentials: true
output-env-credentials: false
output-credentials: true
action-timeout-s: 60
AWS_STEP
cmp -s "$AWS_STEP_NORMALIZED" "$EXPECTED_AWS_STEP" \
  || fail "AWS credential step drifted from the reviewed short-lived, output-only session:\n$(cat "$AWS_STEP_NORMALIZED")"

SIGN_STEP_NORMALIZED="$WF_CODE_DIR/sign.cosign-step.normalized.yml"
EXPECTED_SIGN_STEP="$WF_CODE_DIR/sign.cosign-step.expected.yml"
awk 'NF { line = $0; sub(/^[[:space:]]*/, "", line); sub(/[[:space:]]+$/, "", line); print line }' \
  "$SIGN_STEP" > "$SIGN_STEP_NORMALIZED"
cat > "$EXPECTED_SIGN_STEP" <<'SIGNING_STEP'
- name: Sign SHA256SUMS and attest SLSA provenance
env:
AWS_ACCESS_KEY_ID: ${{ steps.aws.outputs.aws-access-key-id }}
AWS_SECRET_ACCESS_KEY: ${{ steps.aws.outputs.aws-secret-access-key }}
AWS_SESSION_TOKEN: ${{ steps.aws.outputs.aws-session-token }}
AWS_REGION: ${{ secrets.AWS_RELEASE_SIGNING_REGION }}
AWS_DEFAULT_REGION: ${{ secrets.AWS_RELEASE_SIGNING_REGION }}
COSIGN_KMS_URI: ${{ secrets.AWS_RELEASE_KMS_KEY_URI }}
shell: bash
run: |
set -euo pipefail
case "$COSIGN_KMS_URI" in
awskms:///arn:aws:kms:*:*:key/*) ;;
*) printf 'release KMS URI must contain an immutable key ARN\n' >&2; exit 1 ;;
esac
: "${AWS_REGION:?signing session is missing AWS_REGION}"
: "${AWS_DEFAULT_REGION:?signing session is missing AWS_DEFAULT_REGION}"
if [ "$AWS_REGION" != "$AWS_DEFAULT_REGION" ]; then
printf 'AWS_REGION and AWS_DEFAULT_REGION must be identical\n' >&2
exit 1
fi
mkdir signed-release verification
while read -r digest asset; do
cp -- "candidate/$asset" "signed-release/$asset"
done < candidate/SHA256SUMS
cp candidate/SHA256SUMS signed-release/SHA256SUMS
cosign public-key \
--key "$COSIGN_KMS_URI" \
--outfile "$RUNNER_TEMP/aplexica-release.pub"
cmp -s "$RUNNER_TEMP/aplexica-release.pub" aplexica-release.pub
cosign sign-blob --yes \
--key "$COSIGN_KMS_URI" \
--bundle signed-release/SHA256SUMS.sigstore.json \
signed-release/SHA256SUMS
cosign attest-blob --yes \
--key "$COSIGN_KMS_URI" \
--type slsaprovenance1 \
--statement "$RUNNER_TEMP/aplexica.provenance.json" \
--bundle signed-release/aplexica.provenance.sigstore.json
cosign verify-blob \
--key aplexica-release.pub \
--bundle signed-release/SHA256SUMS.sigstore.json \
signed-release/SHA256SUMS
jq -er '.dsseEnvelope.payload' signed-release/aplexica.provenance.sigstore.json \
| base64 --decode > "$RUNNER_TEMP/signed-provenance.json"
cmp -s "$RUNNER_TEMP/aplexica.provenance.json" "$RUNNER_TEMP/signed-provenance.json"
while read -r digest asset; do
cosign verify-blob-attestation \
--type slsaprovenance1 \
--key aplexica-release.pub \
--bundle signed-release/aplexica.provenance.sigstore.json \
"signed-release/$asset"
done < signed-release/SHA256SUMS
go run -mod=readonly ./tools/releaseprovenance \
--verify-bundle signed-release/aplexica.provenance.sigstore.json \
--checksums signed-release/SHA256SUMS \
--commit "$(git rev-parse HEAD)" \
--portal-release packaging/portal-release.json \
--repository "$GITHUB_REPOSITORY" \
--ref "$GITHUB_REF"
cp "$RUNNER_TEMP/aplexica.provenance.json" verification/aplexica.provenance.json
count=$(find signed-release -maxdepth 1 -type f | wc -l | tr -d ' ')
if [ "$count" -ne 13 ]; then
printf 'signed release has %s assets, expected 13\n' "$count" >&2
exit 1
fi
SIGNING_STEP
cmp -s "$SIGN_STEP_NORMALIZED" "$EXPECTED_SIGN_STEP" \
  || fail "KMS signing step drifted from the complete reviewed two-operation program:\n$(cat "$SIGN_STEP_NORMALIZED")"
require_fixed "$SIGN_STEP" 'AWS_REGION: ${{ secrets.AWS_RELEASE_SIGNING_REGION }}'
require_fixed "$SIGN_STEP" 'AWS_DEFAULT_REGION: ${{ secrets.AWS_RELEASE_SIGNING_REGION }}'
require_fixed "$SIGN_STEP" ': "${AWS_REGION:?signing session is missing AWS_REGION}"'
require_fixed "$SIGN_STEP" ': "${AWS_DEFAULT_REGION:?signing session is missing AWS_DEFAULT_REGION}"'
region_secret_bindings="$(grep -Fc -- 'secrets.AWS_RELEASE_SIGNING_REGION' "$SIGN_JOB" || true)"
[ "$region_secret_bindings" -eq 3 ] \
  || fail "sign job must bind secrets.AWS_RELEASE_SIGNING_REGION only to the credentials action and the two signing-step region env vars; found $region_secret_bindings"

for authority_binding in \
  'steps.aws.outputs.aws-access-key-id' \
  'steps.aws.outputs.aws-secret-access-key' \
  'steps.aws.outputs.aws-session-token' \
  'secrets.AWS_RELEASE_KMS_KEY_URI'; do
  binding_count="$(grep -Fc -- "$authority_binding" "$SIGN_JOB" || true)"
  [ "$binding_count" -eq 1 ] \
    || fail "sign job must expose $authority_binding only to the reviewed signing step; found $binding_count occurrences"
done
actual_sign_command="$(extract_continued_command "$SIGN_STEP" 'cosign sign-blob')"
expected_sign_command="$(printf '%s\n' \
  "cosign sign-blob --yes \\" \
  "--key \"\$COSIGN_KMS_URI\" \\" \
  "--bundle signed-release/SHA256SUMS.sigstore.json \\" \
  'signed-release/SHA256SUMS')"
[ "$actual_sign_command" = "$expected_sign_command" ] \
  || fail "sign job must sign exactly signed-release/SHA256SUMS with the canonical command; found:
$actual_sign_command"

# The second KMS signature is a complete SLSA v1 statement, not a predicate.
# `slsaprovenance` alone means deprecated v0.2 in cosign 3.1.1.
actual_attest_command="$(extract_continued_command "$SIGN_STEP" 'cosign attest-blob')"
expected_attest_command="$(printf '%s\n' \
  "cosign attest-blob --yes \\" \
  "--key \"\$COSIGN_KMS_URI\" \\" \
  "--type slsaprovenance1 \\" \
  "--statement \"\$RUNNER_TEMP/aplexica.provenance.json\" \\" \
  '--bundle signed-release/aplexica.provenance.sigstore.json')"
[ "$actual_attest_command" = "$expected_attest_command" ] \
  || fail "sign job must attest the exact reviewed SLSA v1 statement; found:
$actual_attest_command"
if grep -Fq -- '--predicate' "$SIGN_STEP"; then
  fail 'complete release provenance must use cosign attest-blob --statement, never --predicate'
fi

# Verification after publication. The verify job re-downloads what was actually
# published and runs the documented user command against it — the only thing in
# the repository that proves an outsider holding nothing but the public docs
# could authenticate a release. Deleting it leaves every other check green.
VERIFY_STEP="$WF_CODE_DIR/verify.signature-step.yml"
extract_yaml_step "$VERIFY_JOB" 'cosign verify-blob \' "$VERIFY_STEP"
TAP_VERIFY_STEP="$WF_CODE_DIR/tap.signature-step.yml"
extract_yaml_step "$TAP_JOB" 'cosign verify-blob \' "$TAP_VERIFY_STEP"
for verifier in "$VERIFY_JOB" "$TAP_JOB"; do
  count="$(grep -Fc -- 'cosign verify-blob \' "$verifier" || true)"
  [ "$count" -eq 1 ] || fail "$verifier must contain exactly one checksum-signature verification; found $count"
done

# Least privilege at the root. This block is not decoration: deleting it makes
# future jobs inherit mutable repository configuration instead of an explicit
# source-reviewed policy.
grep -A3 '^permissions:' "$WF_CODE" | grep -qE '^[[:space:]]+contents: read' \
  || fail 'release workflow must declare `permissions: contents: read` at the root'

# Tag pushes, and nothing else — asserted as an ALLOW-list over the whole `on:`
# block rather than as a list of trigger names to reject.
#
# A denylist here was the weakest line in this file. `workflow_dispatch` can
# feed an arbitrary branch revision to a KMS-authorized publication path;
# `pull_request` hands contributor-controlled code the `id-token: write` token
# used to assume the signing role; `workflow_call` makes the one file holding
# signing authority invocable from any other workflow in the repository. A
# denylist stops the names someone thought to write down and nothing else.
#
# The load-bearing part is the tag filter itself, which no denylist can assert.
# tools/workflowpolicy scans this file through its `privileged` arm —
# `(hasTrigger(on,"push") && hasTagFilter(on)) || hasTrigger(on,"release")` —
# and returns a workflow that is neither privileged nor untrusted UNSCANNED.
# So replacing `push: tags:` with `push: branches:` does not merely widen the
# trigger: it takes release.yml out of that linter's sight entirely, losing its
# workflow_dispatch rule, its pull_request rule, its self-hosted and
# hosted-runner-literal rules and its reusable-workflow rule in one edit, while
# the workflow keeps `contents: write` + `id-token: write` and now fires on
# every push to main. Both enforcement layers key off the tag filter, which is
# why this gate asserts the filter's presence positively and does not rely on
# workflowpolicy to notice its absence.
#
# Read from the comment-stripped copy: the `on:` block's own prose names
# workflow_dispatch and pull_request in order to explain why they are absent.
WF_ON="$WF_CODE_DIR/release.yml.on-block"
awk '/^on:/ { inblock = 1; next } inblock && /^[^[:space:]]/ { exit } inblock' "$WF_CODE" > "$WF_ON"

triggers="$(awk '/^  [A-Za-z_]+:/ { sub(/:.*/, ""); gsub(/ /, ""); print }' "$WF_ON" | tr '\n' ' ')"
[ "$triggers" = "push " ] \
  || fail "release workflow must trigger on push only; found: [$triggers]"

push_filters="$(awk '/^  push:/ { inpush = 1; next }
  inpush && /^  [A-Za-z_]+:/ { exit }
  inpush && /^    [A-Za-z_]+:/ { sub(/:.*/, ""); gsub(/ /, ""); print }' "$WF_ON" | tr '\n' ' ')"
[ "$push_filters" = "tags " ] \
  || fail "release workflow's push trigger must filter on tags only; found: [$push_filters]"

# And the filter is the exact release-tag glob, not a widened one. `v*` would
# match a pre-release, which this train does not support and whose tag the
# guard job's version check would reject only after the OIDC token had already
# been minted. Quoted scalars are collected from the whole `tags:` region, so
# both the block-sequence and the inline-list spelling are covered, and a
# second pattern added alongside the canonical one shows up as an extra entry.
tags_region="$(awk '/^    tags:/ { intags = 1; print; next }
  intags && /^    [A-Za-z]/ { exit }
  intags { print }' "$WF_ON")"
tag_globs="$(printf '%s\n' "$tags_region" | grep -oE "'[^']*'" | tr '\n' ' ' || true)"
[ "$tag_globs" = "'v[0-9]+.[0-9]+.[0-9]+' " ] \
  || fail "release workflow must filter on exactly the release-tag glob 'v[0-9]+.[0-9]+.[0-9]+'; found: [$tag_globs]"

# Every release job, including sign, uses the private+canonical+not-fork
# select — not a hard fleet pin, not a hard hosted pin, and not a
# visibility-only switch. Private hosted is not in the production OIDC
# allow list, so sign cannot stay on literal ubuntu-latest while the
# repository is private. After a visibility flip the same expression lands
# on GitHub-hosted. Generic `self-hosted` remains forbidden.
reject_regex "$RELEASE_WORKFLOW" 'runs-on:.*self-hosted' 'may run a release job on a generic self-hosted runner'
reject_regex "$WF_CODE" 'runs-on:[[:space:]]*aplexica-' 'pins a fleet runner without the private+canonical+not-fork select'
linux_select="    runs-on: \${{ (github.repository == 'Aplexica/Aplexica' && github.event.repository.fork == false && github.event.repository.visibility == 'private') && 'aplexica-linux' || 'ubuntu-latest' }}"
mac_select="    runs-on: \${{ (github.repository == 'Aplexica/Aplexica' && github.event.repository.fork == false && github.event.repository.visibility == 'private') && 'aplexica-mac' || 'macos-latest' }}"
require_exact_line "$GUARD_JOB" "$linux_select"
require_exact_line "$BUILD_JOB" "$mac_select"
require_exact_line "$SIGN_JOB" "$linux_select"
require_exact_line "$PUBLISH_JOB" "$linux_select"
require_exact_line "$VERIFY_JOB" "$linux_select"
require_exact_line "$TAP_JOB" "$linux_select"

# The exact-line pins above cannot see a seventh job or a reformatted
# expression. Repeat the allow-list here so an extra `runs-on:` is visible
# on all three Test legs, not only on the Security workflowpolicy scan.
bad_runner="$(grep -nE '^[[:space:]]*runs-on:' "$WF_CODE" \
  | grep -vF -e "$linux_select" -e "$mac_select" || true)"
[ -z "$bad_runner" ] || fail "release workflow runner select drifted:
$bad_runner"

# No error-tolerant security steps.
reject_regex "$RELEASE_WORKFLOW" 'continue-on-error' 'lets a release step fail without failing the release'

# GitHub may authorize AWS STS through OIDC, but it must not sign release bytes
# or provenance. Both published signatures are made by the product's KMS key.
for forbidden in 'actions/attest-build-provenance' 'gh attestation' \
  'ATTESTATIONS_ENABLED' 'attestations: write' \
  '--certificate-identity' '--certificate-oidc-issuer'; do
  if grep -Fq -- "$forbidden" "$RELEASE_WORKFLOW"; then
    fail "release workflow contains forbidden GitHub/Fulcio signing marker: $forbidden"
  fi
done

# Public source may name secret interfaces but never reveal live AWS resource
# identifiers, use static access-key secrets, or dump the credential-bearing
# environment. The KMS runtime URI must contain a fixed key ARN, not an alias.
reject_regex "$RELEASE_WORKFLOW" 'arn:aws:[^:]+:[^:]*:[0-9]{12}:' 'contains a literal AWS resource ARN'
reject_regex "$RELEASE_WORKFLOW" 'secrets\.(AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN)' 'uses static AWS access-key secrets'
reject_regex "$RELEASE_WORKFLOW" '(^|[;&|[:space:]])(env|printenv)([;&|[:space:]]|$)|set[[:space:]]+-x|toJSON\([[:space:]]*secrets' 'may print credentials or secrets'
reject_regex "$RELEASE_WORKFLOW" 'awskms:.*alias/' 'permits a retargetable KMS alias'
require_fixed "$SIGN_JOB" 'awskms:///arn:aws:kms:*:*:key/*'
for secret_name in AWS_RELEASE_SIGNING_ROLE_ARN AWS_RELEASE_SIGNING_REGION AWS_RELEASE_ACCOUNT_ID AWS_RELEASE_KMS_KEY_URI; do
  require_fixed "$SIGN_JOB" "secrets.$secret_name"
done

# `if:` is the other way to skip a step quietly, and rejecting only
# continue-on-error above would guard one door while leaving the other open.
# Hanging `if: vars.SIGNING_ENABLED == 'true'` off the signing step would ship
# an entirely unsigned release with every leg of CI green, because an unset
# repository variable is indistinguishable from a deliberately disabled
# feature.
#
# Each condition is bound to the thing it guards, not merely enumerated. An
# allow-list of condition STRINGS is satisfied by moving an already-permitted
# condition onto a step that must never be conditional: hanging the tap's
# `vars.TAP_PUBLISH_ENABLED` gate off `Sign SHA256SUMS` reads as reviewed while
# publishing an unsigned release. So the pair — owner and condition — is what
# gets matched, and no step or job acquires a gate without editing this list
# and saying why.
#
# Indentation is how a job-level `if:` (4 spaces, owned by the enclosing job)
# is told from a step-level one (8 spaces, owned by the enclosing step name).
# Any other indent yields no owner and therefore no allow-list match, which
# fails closed.
#
# Read from the file as written, not the stripped copy: shell `if [ ... ]`
# inside a `run:` block has no colon after `if`, and a commented-out `if:` is
# preceded by `#`, so neither can match.
gated="$(awk '
  /^  [A-Za-z_-]+:[[:space:]]*\r?$/ { job = $0; sub(/^  /, "", job); sub(/:.*/, "", job) }
  /^      -[[:space:]]+name:/ { step = $0; sub(/^[^:]*:[[:space:]]*/, "", step); sub(/[[:space:]]*\r?$/, "", step) }
  /^    if:/  { cond = $0; sub(/^[[:space:]]*if:[[:space:]]*/, "", cond); sub(/[[:space:]]*\r?$/, "", cond)
                printf "%d\tjob:%s\t%s\n", NR, job, cond; next }
  /^        if:/ { cond = $0; sub(/^[[:space:]]*if:[[:space:]]*/, "", cond); sub(/[[:space:]]*\r?$/, "", cond)
                printf "%d\tstep:%s\t%s\n", NR, step, cond; next }
  /^[[:space:]]*if:/ { printf "%d\tunknown-owner\t%s\n", NR, $0 }
' "$RELEASE_WORKFLOW")"
allowed_gates="$(printf '%s\n' \
  "job:tap	vars.TAP_PUBLISH_ENABLED == 'true'")"
# No `grep -v '^$'` filtering the empty case out: under `set -o pipefail` a grep
# that matches nothing would fail the whole substitution, turning "this workflow
# has no conditional steps" into an unreadable abort.
unexpected_if="$(printf '%s\n' "$gated" \
  | while IFS=$'\t' read -r line owner cond; do
      [ -n "$owner" ] || continue
      printf '%s\n' "$allowed_gates" | grep -Fqx -- "$owner	$cond" \
        || printf '%s: %s gated by [%s]\n' "$line" "$owner" "$cond"
    done)"
[ -z "$unexpected_if" ] || fail "release workflow gates a step or job behind an unreviewed condition:
$unexpected_if"

# Named explicitly, and separately from the pair list above, because it is the
# single condition this whole check exists to make impossible: cosign is the
# trust root and must never be skippable. Asserted over the step block rather
# than the `- name:` line, so an `if:` written after `run:` — or before the
# name key — is caught too.
if grep -qE '^[[:space:]]*if:' "$SIGN_STEP"; then
  fail 'the signing step must never be conditional; cosign is the trust root and an unset repository variable is indistinguishable from a deliberately disabled feature'
fi

# Every action pinned to a full 40-character lowercase commit SHA. tools/actionpin
# enforces this in the Security workflow; asserting it here catches it on all
# three Test legs too, where the feedback is immediate.
#
# Both spellings. tools/actionpin unmarshals the YAML and walks mapping keys, so
# it sees `- uses:` and `uses:` alike; a grep that only anchors the second form
# would miss the more common one and quietly buy nothing it claims to buy.
unpinned="$(grep -nE '^[[:space:]]*(-[[:space:]]+)?uses:' "$RELEASE_WORKFLOW" \
  | grep -vE 'uses:[[:space:]]*[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}([[:space:]]|$)' || true)"
[ -z "$unpinned" ] || fail "release workflow has unpinned action references:
$unpinned"

# Flags that turn cosign's guarantees off. There is no legitimate use of any of
# them in this repository — not in the workflow, not as an example, not in a
# footnote — so they are rejected outright rather than reviewed case by case.
#
# The glob is counted first because reject_regex fails OPEN: grep exits 2 on a
# path that does not exist and the `if` reads that as "no match". An unexpanded
# glob would therefore report a clean sweep of nothing.
shopt -s nullglob
install_docs=("$ROOT"/docs/install/*.md)
shopt -u nullglob
[ "${#install_docs[@]}" -gt 0 ] || fail 'docs/install/ has no pages to sweep for insecure cosign flags'
# docs/RELEASING.md is swept alongside them. It is the release operator's
# runbook — the one document whose commands are run by the person holding
# release authority — and this file already treats it as a first-class trust
# surface in the drift guard below. Leaving it out of the flag sweep while
# including it there was an oversight, not a scoping decision.
for file in "$RELEASE_WORKFLOW" "$RELEASING_DOC" "${install_docs[@]}"; do
  reject_regex "$file" '--insecure-ignore-tlog|--insecure-ignore-sct|--allow-insecure-registry|--tlog-upload([=[:space:]]+)false|--use-signing-config([=[:space:]]+)false|--signing-config|--fulcio-url|--rekor-url|--tsa-server-url|--timestamp-server-url' \
    'names a cosign flag that disables verification'
done

# ---------------------------------------------------------------------------
# Drift guard, user verification. Documentation and post-publication CI expose
# one byte-identical public interface. This compares the complete fail-closed
# block, not selected flags.
VERIFY_DOC_BLOCK="$WF_CODE_DIR/verify-doc.user-command.sh"
RELEASING_DOC_BLOCK="$WF_CODE_DIR/releasing-doc.user-command.sh"
CI_VERIFY_BLOCK="$WF_CODE_DIR/verify-job.user-command.sh"
extract_bash_fence "$VERIFY_DOC" "bash -eu -o pipefail <<'VERIFY'" "$VERIFY_DOC_BLOCK"
extract_bash_fence "$RELEASING_DOC" "bash -eu -o pipefail <<'VERIFY'" "$RELEASING_DOC_BLOCK"
extract_heredoc "$VERIFY_STEP" "bash -eu -o pipefail <<'VERIFY'" 'VERIFY' "$CI_VERIFY_BLOCK"
if ! cmp -s "$VERIFY_DOC_BLOCK" "$RELEASING_DOC_BLOCK"; then
  diff -u "$VERIFY_DOC_BLOCK" "$RELEASING_DOC_BLOCK" >&2 || true
  fail 'the Unix user-verification command must be byte-identical in docs/install/verify.md and docs/RELEASING.md'
fi
if ! cmp -s "$VERIFY_DOC_BLOCK" "$CI_VERIFY_BLOCK"; then
  diff -u "$VERIFY_DOC_BLOCK" "$CI_VERIFY_BLOCK" >&2 || true
  fail 'the verify job must run the byte-identical documented public verification block'
fi

# Pin failure semantics and both public bundles even if all three copies drift
# together in one change.
require_exact_line "$VERIFY_DOC_BLOCK" 'curl -fLO "$BASE/SHA256SUMS"'
require_exact_line "$VERIFY_DOC_BLOCK" 'curl -fLO "$BASE/SHA256SUMS.sigstore.json"'
require_exact_line "$VERIFY_DOC_BLOCK" 'curl -fLO "$BASE/aplexica.provenance.sigstore.json"'
require_exact_line "$VERIFY_DOC_BLOCK" 'curl -fLO "$BASE/$ASSET"'

documented_verify_command="$(extract_continued_command "$VERIFY_DOC_BLOCK" 'cosign verify-blob')"
expected_verify_command="$(printf '%s\n' \
  "cosign verify-blob \\" \
  "--key aplexica-release.pub \\" \
  "--bundle SHA256SUMS.sigstore.json \\" \
  'SHA256SUMS')"
[ "$documented_verify_command" = "$expected_verify_command" ] \
  || fail "the public documentation must carry the exact public-key verification command; found:
$documented_verify_command"
documented_attest_command="$(extract_continued_command "$VERIFY_DOC_BLOCK" 'cosign verify-blob-attestation')"
expected_attest_verify="$(printf '%s\n' \
  "cosign verify-blob-attestation \\" \
  "--type slsaprovenance1 \\" \
  "--key aplexica-release.pub \\" \
  "--bundle aplexica.provenance.sigstore.json \\" \
  '"$ASSET"')"
[ "$documented_attest_command" = "$expected_attest_verify" ] \
  || fail "public documentation must verify the exact KMS SLSA v1 bundle; found:
$documented_attest_command"

documented_policy_command="$(extract_continued_command "$VERIFY_DOC_BLOCK" 'go run -mod=readonly ./tools/releaseprovenance')"
expected_policy_command="$(printf '%s\n' \
  'go run -mod=readonly ./tools/releaseprovenance \' \
  '--verify-bundle aplexica.provenance.sigstore.json \' \
  '--checksums SHA256SUMS \' \
  '--commit "$(git rev-parse HEAD)" \' \
  '--portal-release packaging/portal-release.json \' \
  '--repository Aplexica/Aplexica \' \
  '--ref "refs/tags/v$VERSION"')"
[ "$documented_policy_command" = "$expected_policy_command" ] \
  || fail "public documentation must enforce the exact provenance policy; found:
$documented_policy_command"

# The tap is a second consumer of the published signature. Its paths differ
# because it downloads into `published/`, but it must use the same public key
# and must never reach back into KMS or a secret to verify.
tap_verify_command="$(extract_continued_command "$TAP_VERIFY_STEP" 'cosign verify-blob')"
tap_verify_key_args="$(printf '%s\n' "$tap_verify_command" \
  | tr '\n' ' ' \
  | grep -oE -- '--key[[:space:]]+[^[:space:]\\]+' || true)"
[ "$tap_verify_key_args" = '--key aplexica-release.pub' ] \
  || fail "the tap verification step must use exactly --key aplexica-release.pub; found: [$tap_verify_key_args]"
if printf '%s\n' "$documented_verify_command" "$documented_attest_command" "$tap_verify_command" \
  | grep -Eiq -- 'COSIGN_KMS_URI|secrets\.|awskms:|arn:aws:|--certificate-(identity|oidc-issuer)'; then
  fail 'verification must use only aplexica-release.pub, never a certificate identity, KMS URI, AWS ARN, or secret'
fi

# The raw statement is retained only in the private workflow artifact so the
# publisher can compare it with the DSSE payload. It must never become release
# asset fourteen.
assert_before "$PUBLISH_JOB" 'name: Reverify every byte without AWS credentials' 'name: Publish the release'
assert_before "$PUBLISH_JOB" '--verify-bundle signed-release/aplexica.provenance.sigstore.json' '${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/releases'
require_fixed "$PUBLISH_JOB" 'signed-release/aplexica.provenance.sigstore.json'
if grep -Eq 'assets=.*aplexica\.provenance\.json|signed-release/aplexica\.provenance\.json' "$PUBLISH_JOB"; then
  fail 'the unsigned provenance statement must not be a GitHub Release asset'
fi
require_fixed "$PUBLISH_JOB" '-ne 13'

# Publication authority receives no AWS credential, so this is the last
# boundary that can stop a corrupted handoff before public exposure. Pin its
# complete executable body: checking only the semantic verifier does not prove
# the checksum signature, bytes, DSSE payload, or ten artifact attestations.
PUBLISH_VERIFY_STEP="$WF_CODE_DIR/publish.reverify-step.yml"
PUBLISH_VERIFY_STEP_NORMALIZED="$WF_CODE_DIR/publish.reverify-step.normalized.yml"
EXPECTED_PUBLISH_VERIFY_STEP="$WF_CODE_DIR/publish.reverify-step.expected.yml"
extract_yaml_step "$PUBLISH_JOB" 'name: Reverify every byte without AWS credentials' "$PUBLISH_VERIFY_STEP"
awk 'NF { line = $0; sub(/^[[:space:]]*/, "", line); sub(/[[:space:]]+$/, "", line); print line }' \
  "$PUBLISH_VERIFY_STEP" > "$PUBLISH_VERIFY_STEP_NORMALIZED"
cat > "$EXPECTED_PUBLISH_VERIFY_STEP" <<'PUBLISH_VERIFY_STEP'
- name: Reverify every byte without AWS credentials
shell: bash
run: |
set -euo pipefail
count=$(find signed-release -maxdepth 1 -type f | wc -l | tr -d ' ')
if [ "$count" -ne 13 ]; then
printf 'signed release has %s assets, expected 13\n' "$count" >&2
exit 1
fi
cosign verify-blob \
--key aplexica-release.pub \
--bundle signed-release/SHA256SUMS.sigstore.json \
signed-release/SHA256SUMS
( cd signed-release && shasum -a 256 --check SHA256SUMS )
jq -er '.dsseEnvelope.payload' signed-release/aplexica.provenance.sigstore.json \
| base64 --decode > "$RUNNER_TEMP/signed-provenance.json"
cmp -s verification/aplexica.provenance.json "$RUNNER_TEMP/signed-provenance.json"
while read -r digest asset; do
cosign verify-blob-attestation \
--type slsaprovenance1 \
--key aplexica-release.pub \
--bundle signed-release/aplexica.provenance.sigstore.json \
"signed-release/$asset"
done < signed-release/SHA256SUMS
go run -mod=readonly ./tools/releaseprovenance \
--verify-bundle signed-release/aplexica.provenance.sigstore.json \
--checksums signed-release/SHA256SUMS \
--commit "$(git rev-parse HEAD)" \
--portal-release packaging/portal-release.json \
--repository "$GITHUB_REPOSITORY" \
--ref "$GITHUB_REF"
PUBLISH_VERIFY_STEP
cmp -s "$PUBLISH_VERIFY_STEP_NORMALIZED" "$EXPECTED_PUBLISH_VERIFY_STEP" \
  || fail "publisher byte-and-cryptography verification step drifted:\n$(cat "$PUBLISH_VERIFY_STEP_NORMALIZED")"

# Bind publication to the complete allow-list. The runtime count prevents a
# missing platform build, but a count alone accepts twelve intended files plus
# one attacker-added file. Likewise, checking only for the seven expected
# entries accepts an eighth glob. Compare the entire source array and the
# reviewed curl create + 13 uploads + GET count sequence.
PUBLISH_STEP="$WF_CODE_DIR/publish.release-step.yml"
extract_yaml_step "$PUBLISH_JOB" 'name: Publish the release' "$PUBLISH_STEP"
PUBLISH_STEP_NORMALIZED="$WF_CODE_DIR/publish.release-step.normalized.yml"
EXPECTED_PUBLISH_STEP="$WF_CODE_DIR/publish.release-step.expected.yml"
awk 'NF { line = $0; sub(/^[[:space:]]*/, "", line); sub(/[[:space:]]+$/, "", line); print line }' \
  "$PUBLISH_STEP" > "$PUBLISH_STEP_NORMALIZED"
cat > "$EXPECTED_PUBLISH_STEP" <<'PUBLISH_STEP'
- name: Publish the release
shell: bash
env:
GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
run: |
set -euo pipefail
shopt -s nullglob
: "${GITHUB_TOKEN:?GITHUB_TOKEN is missing}"
: "${GITHUB_REPOSITORY:?}"
: "${GITHUB_REF_NAME:?}"
: "${GITHUB_API_URL:?}"
notes="$RUNNER_TEMP/release-notes/release-notes.md"
[ -s "$notes" ] || { printf 'release notes are missing or empty\n' >&2; exit 1; }
assets=(
signed-release/aplexica-*.tar.gz
signed-release/aplexica-*.zip
signed-release/aplexica_*.deb
signed-release/aplexica.sbom.cdx.json
signed-release/SHA256SUMS
signed-release/SHA256SUMS.sigstore.json
signed-release/aplexica.provenance.sigstore.json
)
if [ "${#assets[@]}" -ne 13 ]; then
printf 'expected 13 release assets, found %s:\n' "${#assets[@]}" >&2
printf '  %s\n' "${assets[@]}" >&2
exit 1
fi
payload=$(jq -n --arg tag "$GITHUB_REF_NAME" --arg name "Aplexica $GITHUB_REF_NAME" --rawfile body "$notes" \
'{tag_name:$tag,name:$name,body:$body,draft:false,prerelease:false}')
created=$(curl -fsS -X POST \
-H "Authorization: Bearer ${GITHUB_TOKEN}" \
-H "Accept: application/vnd.github+json" \
-H "X-GitHub-Api-Version: 2022-11-28" \
-H "Content-Type: application/json" \
--data "$payload" \
"${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/releases")
printf '%s' "$created" | jq -e --arg tag "$GITHUB_REF_NAME" '.draft == false and .prerelease == false and .tag_name == $tag' >/dev/null
release_id=$(printf '%s' "$created" | jq -er '.id')
[ -n "$release_id" ] || { printf 'release create returned no id\n' >&2; exit 1; }
for f in "${assets[@]}"; do
name=$(basename -- "$f")
printf '%s' "$name" | grep -Eq '^[A-Za-z0-9._+-]+$' \
|| { printf 'refusing to upload an unsafe asset name: %s\n' "$name" >&2; exit 1; }
curl -fsS -X POST \
-H "Authorization: Bearer ${GITHUB_TOKEN}" \
-H "Content-Type: application/octet-stream" \
--data-binary @"$f" \
"https://uploads.github.com/repos/${GITHUB_REPOSITORY}/releases/${release_id}/assets?name=${name}" \
>/dev/null
done
listed=$(curl -fsS \
-H "Authorization: Bearer ${GITHUB_TOKEN}" \
-H "Accept: application/vnd.github+json" \
-H "X-GitHub-Api-Version: 2022-11-28" \
"${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/releases/${release_id}")
count=$(printf '%s' "$listed" | jq -e '.assets | length')
[ "$count" -eq 13 ] || { printf 'published asset count is %s, expected 13\n' "$count" >&2; exit 1; }
PUBLISH_STEP
cmp -s "$PUBLISH_STEP_NORMALIZED" "$EXPECTED_PUBLISH_STEP" \
  || fail "GitHub Release publication step drifted from the reviewed curl allow-list:\n$(cat "$PUBLISH_STEP_NORMALIZED")"
actual_asset_array="$(awk '
  /^[[:space:]]*assets=\([[:space:]]*$/ { copying = 1 }
  copying {
    line = $0
    sub(/\r$/, "", line)
    sub(/^[[:space:]]*/, "", line)
    print line
    if (line == ")") exit
  }
' "$PUBLISH_STEP")"
expected_asset_array="$(printf '%s\n' \
  'assets=(' \
  'signed-release/aplexica-*.tar.gz' \
  'signed-release/aplexica-*.zip' \
  'signed-release/aplexica_*.deb' \
  'signed-release/aplexica.sbom.cdx.json' \
  'signed-release/SHA256SUMS' \
  'signed-release/SHA256SUMS.sigstore.json' \
  'signed-release/aplexica.provenance.sigstore.json' \
  ')')"
[ "$actual_asset_array" = "$expected_asset_array" ] \
  || fail "publish job release-asset allow-list drifted; found:
$actual_asset_array"
release_mutations="$(grep -Ec -- 'gh release (create|upload|edit|delete)' "$PUBLISH_JOB" || true)"
[ "$release_mutations" -eq 0 ] \
  || fail "publish job must contain zero gh release mutations; found $release_mutations"
publish_token_bindings="$(grep -Fc -- 'secrets.GITHUB_TOKEN' "$PUBLISH_JOB" || true)"
[ "$publish_token_bindings" -eq 1 ] \
  || fail "publish job must expose GITHUB_TOKEN only to the exact publication step; found $publish_token_bindings occurrences"
if grep -Eq -- 'gh[[:space:]]+api|github[[:space:]]*\[|github\.token' "$PUBLISH_JOB"; then
  fail 'publish job contains an alternate GitHub API or token path outside the reviewed curl publication sequence'
fi
create_url_count="$(grep -Fc -- '${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/releases' "$PUBLISH_JOB" || true)"
get_url_count="$(grep -Fc -- '${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/releases/${release_id}' "$PUBLISH_JOB" || true)"
upload_url_count="$(grep -Fc -- 'https://uploads.github.com/repos/${GITHUB_REPOSITORY}/releases/${release_id}/assets?name=${name}' "$PUBLISH_JOB" || true)"
uploads_host_count="$(grep -Fc -- 'uploads.github.com' "$PUBLISH_JOB" || true)"
# create URL is a prefix of GET; require one create POST + one GET + one upload URL source.
[ "$get_url_count" -eq 1 ] \
  || fail "publish job must contain exactly one GET release URL; found $get_url_count"
[ "$create_url_count" -eq 2 ] \
  || fail "publish job must contain create POST + GET (two GITHUB_API_URL release URLs); found $create_url_count"
[ "$upload_url_count" -eq 1 ] && [ "$uploads_host_count" -eq 1 ] \
  || fail "publish job must contain exactly one reviewed uploads.github.com asset URL; found upload=$upload_url_count host=$uploads_host_count"
if grep -Eq -- 'api\.github\.com' "$PUBLISH_JOB"; then
  fail 'publish job must use GITHUB_API_URL, not a hardcoded api.github.com host'
fi

# The post-publication job is the independent user view. It anonymously
# enumerates the remote release and rejects any omitted, renamed, duplicate, or
# extra asset before verifying the four downloaded trust artifacts.
REMOTE_ASSET_STEP="$WF_CODE_DIR/verify.remote-assets-step.yml"
REMOTE_ASSET_STEP_NORMALIZED="$WF_CODE_DIR/verify.remote-assets-step.normalized.yml"
EXPECTED_REMOTE_ASSET_STEP="$WF_CODE_DIR/verify.remote-assets-step.expected.yml"
extract_yaml_step "$VERIFY_JOB" 'https://api.github.com/repos/Aplexica/Aplexica/releases/tags/v$VERSION' "$REMOTE_ASSET_STEP"
awk 'NF { line = $0; sub(/^[[:space:]]*/, "", line); sub(/[[:space:]]+$/, "", line); print line }' \
  "$REMOTE_ASSET_STEP" > "$REMOTE_ASSET_STEP_NORMALIZED"
cat > "$EXPECTED_REMOTE_ASSET_STEP" <<'REMOTE_ASSET_STEP'
- name: Require the exact public release asset set
shell: bash
run: |
set -euo pipefail
metadata=$(curl -fsSL \
-H 'Accept: application/vnd.github+json' \
-H 'X-GitHub-Api-Version: 2022-11-28' \
"https://api.github.com/repos/Aplexica/Aplexica/releases/tags/v$VERSION")
actual=$(printf '%s' "$metadata" | jq -er --arg tag "v$VERSION" '
select(.tag_name == $tag and .draft == false and .prerelease == false)
| [.assets[].name]
| sort
| .[]
')
expected=$(printf '%s\n' \
"aplexica-$VERSION-darwin-amd64.tar.gz" \
"aplexica-$VERSION-darwin-arm64.tar.gz" \
"aplexica-$VERSION-linux-amd64.tar.gz" \
"aplexica-$VERSION-linux-arm64.tar.gz" \
"aplexica-$VERSION-source.tar.gz" \
"aplexica-$VERSION-windows-amd64.zip" \
"aplexica-$VERSION-windows-arm64.zip" \
"aplexica_$VERSION_amd64.deb" \
"aplexica_$VERSION_arm64.deb" \
'aplexica.provenance.sigstore.json' \
'aplexica.sbom.cdx.json' \
'SHA256SUMS' \
'SHA256SUMS.sigstore.json' \
| LC_ALL=C sort)
if [ "$actual" != "$expected" ]; then
printf 'published release asset set drifted\nexpected:\n%s\nactual:\n%s\n' \
"$expected" "$actual" >&2
exit 1
fi
REMOTE_ASSET_STEP
cmp -s "$REMOTE_ASSET_STEP_NORMALIZED" "$EXPECTED_REMOTE_ASSET_STEP" \
  || fail "post-publication exact asset-set assertion drifted:\n$(cat "$REMOTE_ASSET_STEP_NORMALIZED")"
assert_before "$VERIFY_JOB" 'name: Require the exact public release asset set' 'name: Verify the signature, digest, and provenance'

# ---------------------------------------------------------------------------
# Drift guard, asset names. One template in .goreleaser.yaml decides what every
# release asset is called, and four consumers spell out their own rendering of
# it: the documented curl, the Homebrew formula's url, the WinGet InstallerUrl,
# and the apt instructions. Nothing else in the repository compares them. This
# project has already shipped that disagreement twice — installer scripts and
# docs written against `aplexica-v...` while the build produced `aplexica-...`
# — and the release workflow's verify job only catches it after `gh release
# create` has already published, while a docs-only drift is never caught at all.
#
# Two templates, because .deb keeps Debian's convention on purpose: hyphens and
# GOARCH for archives, underscores and dpkg arch for packages.
require_fixed "$GORELEASER" "name_template: 'aplexica-{{ .Version }}-{{ .Os }}-{{ .Arch }}'"
require_fixed "$GORELEASER" "file_name_template: 'aplexica_{{ .Version }}_{{ .Arch }}'"
require_fixed "$VERIFY_DOC" 'aplexica-$VERSION-darwin-arm64.tar.gz'
require_fixed "$HOMEBREW_FORMULA" 'aplexica-#{version}-darwin-arm64.tar.gz'
require_fixed "$WINGET_INSTALLER" 'aplexica-VERSION_PLACEHOLDER-windows-amd64.zip'
require_fixed "$ROOT/docs/install/apt.md" 'aplexica_${VERSION}_${ARCH}.deb'

GORELEASER_SOURCE="$WF_CODE_DIR/goreleaser.source.yml"
extract_yaml_top_level "$GORELEASER" source "$GORELEASER_SOURCE"
source_contract="$(sed 's/#.*$//' "$GORELEASER_SOURCE" \
  | awk 'NF { line = $0; sub(/^[[:space:]]*/, "", line); sub(/[[:space:]]+$/, "", line); print line }')"
expected_source_contract="$(printf '%s\n' \
  'source:' \
  'enabled: true' \
  "name_template: '{{ .ProjectName }}-{{ .Version }}-source'" \
  "prefix_template: '{{ .ProjectName }}-{{ .Version }}/'")"
[ "$source_contract" = "$expected_source_contract" ] \
  || fail "GoReleaser source archive must be enabled, canonically named, and rooted under one versioned directory; found:\n$source_contract"

# Archive interiors are part of the release contract, not an incidental
# GoReleaser default. Pin the complete input set here: four build definitions
# collapse to the three required program basenames for each target, followed
# by exactly five documentation files. Mode keys are forbidden in this block
# because GoReleaser's reviewed defaults are the contract — 0755 for build
# outputs and 0644 for files — and one archive-level override would silently
# apply the same mode to both classes.
GORELEASER_BUILDS="$WF_CODE_DIR/goreleaser.builds.yml"
GORELEASER_ARCHIVES="$WF_CODE_DIR/goreleaser.archives.yml"
extract_yaml_top_level "$GORELEASER" builds "$GORELEASER_BUILDS"
extract_yaml_top_level "$GORELEASER" archives "$GORELEASER_ARCHIVES"

GORELEASER_ARCHIVES_CODE="$WF_CODE_DIR/goreleaser.archives.comments-stripped.yml"
sed 's/#.*$//' "$GORELEASER_ARCHIVES" | awk 'NF { sub(/[[:space:]]+$/, ""); print }' > "$GORELEASER_ARCHIVES_CODE"
expected_archives_code="$(cat <<'ARCHIVES'
archives:
  - id: aplexica-archives
    ids:
      - aplexica
      - aplexica-status
      - aplexicatray
      - aplexicatray-windows
    name_template: 'aplexica-{{ .Version }}-{{ .Os }}-{{ .Arch }}'
    formats:
      - tar.gz
    format_overrides:
      - goos: windows
        formats:
          - zip
    builds_info:
      owner: root
      group: root
      mtime: '{{ .CommitDate }}'
    files:
      - src: LICENSE
        info:
          owner: root
          group: root
          mtime: '{{ .CommitDate }}'
      - src: LICENSE-EXCEPTIONS.md
        info:
          owner: root
          group: root
          mtime: '{{ .CommitDate }}'
      - src: README.md
        info:
          owner: root
          group: root
          mtime: '{{ .CommitDate }}'
      - src: SECURITY.md
        info:
          owner: root
          group: root
          mtime: '{{ .CommitDate }}'
      - src: CHANGELOG.md
        info:
          owner: root
          group: root
          mtime: '{{ .CommitDate }}'
ARCHIVES
)"
actual_archives_code="$(cat "$GORELEASER_ARCHIVES_CODE")"
[ "$actual_archives_code" = "$expected_archives_code" ] \
  || fail ".goreleaser.yaml archive structure drifted from the exact three-binary/five-document contract"

for build_id in aplexica aplexica-status aplexicatray aplexicatray-windows; do
  count="$(grep -Fcx -- "  - id: $build_id" "$GORELEASER_BUILDS" || true)"
  [ "$count" -eq 1 ] \
    || fail ".goreleaser.yaml must define build '$build_id' exactly once; found $count"
done
aplexica_binaries="$(grep -Fcx -- '    binary: aplexica' "$GORELEASER_BUILDS" || true)"
status_binaries="$(grep -Fcx -- '    binary: aplexica-status' "$GORELEASER_BUILDS" || true)"
tray_binaries="$(grep -Fcx -- '    binary: aplexicatray' "$GORELEASER_BUILDS" || true)"
[ "$aplexica_binaries" -eq 1 ] && [ "$status_binaries" -eq 1 ] && [ "$tray_binaries" -eq 2 ] \
  || fail ".goreleaser.yaml build binaries must be aplexica once, aplexica-status once, and aplexicatray twice"

build_entrypoints="$(awk '
  function emit() {
    if (id != "") print id "|" main "|" binary "|" tag
  }
  /^  - id: / {
    emit()
    id=$3
    main=""
    binary=""
    tag=""
    want_tag=0
    next
  }
  id != "" && $1 == "main:" { main=$2 }
  id != "" && $1 == "binary:" { binary=$2 }
  id != "" && $1 == "tags:" { want_tag=1; next }
  id != "" && want_tag && $1 == "-" { tag=$2; want_tag=0 }
  END { emit() }
' "$GORELEASER_BUILDS")"
expected_build_entrypoints="$(printf '%s\n' \
  'aplexica|./cmd/aplexica|aplexica|release' \
  'aplexica-status|./cmd/aplexica|aplexica-status|release' \
  'aplexicatray|./cmd/aplexicatray|aplexicatray|tray' \
  'aplexicatray-windows|./cmd/aplexicatray|aplexicatray|tray')"
[ "$build_entrypoints" = "$expected_build_entrypoints" ] \
  || fail ".goreleaser.yaml build id/main/binary mapping drifted; found:
$build_entrypoints"

archive_ids="$(awk '
  /^    ids:[[:space:]]*$/ { in_ids = 1; next }
  in_ids && /^    [A-Za-z0-9_-]+:/ { exit }
  in_ids && /^      - / { sub(/^      - /, ""); print }
' "$GORELEASER_ARCHIVES")"
expected_archive_ids="$(printf '%s\n' aplexica aplexica-status aplexicatray aplexicatray-windows)"
[ "$archive_ids" = "$expected_archive_ids" ] \
  || fail ".goreleaser.yaml archive build set drifted; found:
$archive_ids"

archive_files="$(awk '
  /^    files:[[:space:]]*$/ { in_files = 1; next }
  in_files && /^      - src: / { sub(/^      - src: /, ""); print }
' "$GORELEASER_ARCHIVES")"
expected_archive_files="$(printf '%s\n' LICENSE LICENSE-EXCEPTIONS.md README.md SECURITY.md CHANGELOG.md)"
[ "$archive_files" = "$expected_archive_files" ] \
  || fail ".goreleaser.yaml archives must contain exactly the five canonical documentation files; found:
$archive_files"

build_info_owners="$(grep -Fcx -- '      owner: root' "$GORELEASER_ARCHIVES" || true)"
build_info_groups="$(grep -Fcx -- '      group: root' "$GORELEASER_ARCHIVES" || true)"
build_info_mtimes="$(grep -Fcx -- "      mtime: '{{ .CommitDate }}'" "$GORELEASER_ARCHIVES" || true)"
archive_owners="$(grep -Fcx -- '          owner: root' "$GORELEASER_ARCHIVES" || true)"
archive_groups="$(grep -Fcx -- '          group: root' "$GORELEASER_ARCHIVES" || true)"
archive_mtimes="$(grep -Fcx -- "          mtime: '{{ .CommitDate }}'" "$GORELEASER_ARCHIVES" || true)"
[ "$build_info_owners" -eq 1 ] && [ "$build_info_groups" -eq 1 ] && [ "$build_info_mtimes" -eq 1 ] && \
  [ "$archive_owners" -eq 5 ] && [ "$archive_groups" -eq 5 ] && [ "$archive_mtimes" -eq 5 ] \
  || fail ".goreleaser.yaml must pin root:root and commit mtime on builds_info plus all five archive files"
if grep -Eq '^[[:space:]]+mode:' "$GORELEASER_ARCHIVES"; then
  fail '.goreleaser.yaml archives must preserve GoReleaser defaults: binaries 0755 and files 0644'
fi
archive_tar_formats="$(grep -Fcx -- '      - tar.gz' "$GORELEASER_ARCHIVES" || true)"
archive_zip_formats="$(grep -Fcx -- '          - zip' "$GORELEASER_ARCHIVES" || true)"
[ "$archive_tar_formats" -eq 1 ] && [ "$archive_zip_formats" -eq 1 ] \
  || fail '.goreleaser.yaml archive formats must be tar.gz with one Windows zip override'

# The deliberately UNVERSIONED names, pinned for the opposite reason: the
# per-tag URL prefix already disambiguates them, and their stability is the
# whole reason the documented verify command is copy-pasteable across releases.
# Renaming the checksum file fails the workflow loudly (its `dist/SHA256SUMS`
# paths are hardcoded), but it silently rots every command already pasted into
# a terminal, and the SBOM name is not hardcoded anywhere that would complain.
require_fixed "$GORELEASER" 'name_template: "SHA256SUMS"'
require_fixed "$GORELEASER" 'aplexica.sbom.cdx.json'
require_fixed "$VERIFY_DOC" 'SHA256SUMS.sigstore.json'
require_fixed "$RELEASING_DOC" 'SHA256SUMS.sigstore.json'
require_fixed "$VERIFY_DOC" 'aplexica.provenance.sigstore.json'
require_fixed "$RELEASING_DOC" 'aplexica.provenance.sigstore.json'

# Only GoReleaser stamps the official release-train marker. Ordinary Makefile
# builds can carry a tag-shaped version and a full Git commit, but they remain
# source builds and must never advance the updater rollback floor.
release_train_stamps="$(grep -Fc -- 'internal/version.ReleaseTrain=github-actions-aws-kms-v1' "$GORELEASER" || true)"
[ "$release_train_stamps" -eq 4 ] \
  || fail ".goreleaser.yaml must stamp the official release train in all four binary definitions; found $release_train_stamps"
if grep -Fq -- 'internal/version.ReleaseTrain=' "$ROOT/Makefile"; then
  fail 'the ordinary Makefile build must not stamp the official release train'
fi

# ---------------------------------------------------------------------------
# Residue sweep: unsupported release-authority artifacts must not create a
# second trust path beside the KMS-signed release contract.
#
# NAME COLLISION HAZARD: the exact .json filenames are matched, never a bare
# `release.inventory` prefix. internal/plugin/proto and internal/plugin/secureexec
# use release.inventory.cbor for the bundled Cloud-plugin trust. A prefix
# match would incorrectly reject that independent protocol object.
if ! command -v git >/dev/null 2>&1 || ! git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  fail 'residue sweep needs a git checkout; a silently skipped sweep is the failure it exists to prevent'
fi
# The self-exemption is derived from this script's own name rather than
# hardcoded, so a RENAME cannot silently detach it and leave the sweep failing
# on the very text that defines it. The `packaging/scripts/` prefix is still
# literal, so a MOVE does detach it — fail-closed in that direction too, since
# the sweep would then fire on this file's own vocabulary and say so.
self_exempt="$(printf 'packaging/scripts/%s' "$(basename "${BASH_SOURCE[0]}")" | sed 's/\./\\./g')"
residue_exempt="^($self_exempt)\$"
for term in 'release.inventory.json' 'release.inventory.sig.json' 'aplexica-release-inventory'; do
  # --untracked so a file that has not been `git add`ed yet is swept too. In CI
  # actions/checkout produces a fully tracked tree and this changes nothing, but
  # locally the release-critical files are often the newest ones, and residue in
  # a file nobody has staged is exactly the residue worth catching early.
  #
  # It does NOT reach .gitignore'd paths — dist/ above all — and that is
  # deliberate: build output is regenerated from the tree this sweep does
  # cover, so residue can only ever arrive there by way of a source file that
  # is swept. Nothing ships from an ignored path.
  hits="$(cd "$ROOT" && git grep --untracked -lIF -e "$term" | grep -Ev "$residue_exempt" || true)"
  [ -z "$hits" ] || fail "unsupported release-authority literal '$term' survives in: $(printf '%s' "$hits" | tr '\n' ' ')"
done

# The literal sweep above catches unsupported authority vocabulary. It does
# not catch unsupported artifacts coming back under their own names, which
# spell nothing forbidden: a recreated packaging/scripts/install.sh contains the
# string "install.sh" and nothing else the sweep looks for, and a build entry
# emitting aplexica-installer-helper-darwin-arm64 is likewise invisible to it.
# Those four names head the "must never be published again" list, and this gate
# is the mechanism that makes their deletion permanent, so it asserts their
# absence directly.
#
# Curl-pipe-shell installers are forbidden because no independently
# authenticated script-install contract exists.
for unsupported in packaging/scripts/install.sh packaging/scripts/install.ps1 \
  cmd/aplexica-installer-helper cmd/aplexica-direct-launcher; do
  [ ! -e "$ROOT/$unsupported" ] || fail "unsupported installer artifact '$unsupported' has been recreated"
done

# And the names themselves, scoped to the two files that decide what a release
# publishes. Deliberately NOT a whole-tree sweep: CHANGELOG.md records the
# removal by name and docs may explain it, and neither can publish anything.
# .goreleaser.yaml and .github/workflows/ can.
for term in 'aplexica-installer-helper' 'aplexica-direct-launcher'; do
  hits="$(cd "$ROOT" && git grep --untracked -lIF -e "$term" -- .goreleaser.yaml .github/workflows || true)"
  [ -z "$hits" ] || fail "unsupported companion binary '$term' is named by the release path in: $(printf '%s' "$hits" | tr '\n' ' ')"
done


# ---------------------------------------------------------------------------
# Break-and-restore: a missing signing-region env must fail the reviewed step
# contract, and putting the env back must pass. Mutations run against copies
# of release.yml; the production file is never rewritten.
# ---------------------------------------------------------------------------
normalized_sign_step_from_workflow() {
  local workflow="$1"
  local dest="$2"
  local work="$WF_CODE_DIR/break-restore.work"
  mkdir -p "$work"
  awk '
    /^[[:space:]]*#/ { print ""; next }
    {
      line = $0
      if (line ~ /^[[:space:]]*(-[[:space:]]+)?uses:/) {
        sub(/[[:space:]]+#.*$/, "", line)
      }
      print line
    }
  ' "$workflow" > "$work/code.yml"
  extract_yaml_job "$work/code.yml" sign "$work/sign.yml"
  extract_yaml_step "$work/sign.yml" 'cosign sign-blob' "$work/step.yml"
  awk 'NF { line = $0; sub(/^[[:space:]]*/, "", line); sub(/[[:space:]]+$/, "", line); print line }' \
    "$work/step.yml" > "$dest"
}

expect_sign_step_mismatch() {
  local label="$1"
  local workflow="$2"
  local normalized="$WF_CODE_DIR/break-restore.$label.normalized.yml"
  normalized_sign_step_from_workflow "$workflow" "$normalized"
  if cmp -s "$normalized" "$EXPECTED_SIGN_STEP"; then
    fail "break-restore: $label still matched the reviewed signing step"
  fi
}

expect_sign_step_match() {
  local label="$1"
  local workflow="$2"
  local normalized="$WF_CODE_DIR/break-restore.$label.normalized.yml"
  normalized_sign_step_from_workflow "$workflow" "$normalized"
  cmp -s "$normalized" "$EXPECTED_SIGN_STEP" \
    || fail "break-restore: $label did not restore the reviewed signing step"
}

broken="$WF_CODE_DIR/release.missing-AWS_REGION.yml"
awk '
  $0 ~ /^          AWS_REGION: / { next }
  { print }
' "$RELEASE_WORKFLOW" > "$broken"
expect_sign_step_mismatch 'missing-AWS_REGION' "$broken"
expect_sign_step_match 'restore-after-missing-AWS_REGION' "$RELEASE_WORKFLOW"

broken="$WF_CODE_DIR/release.missing-AWS_DEFAULT_REGION.yml"
awk '
  $0 ~ /^          AWS_DEFAULT_REGION: / { next }
  { print }
' "$RELEASE_WORKFLOW" > "$broken"
expect_sign_step_mismatch 'missing-AWS_DEFAULT_REGION' "$broken"
expect_sign_step_match 'restore-after-missing-AWS_DEFAULT_REGION' "$RELEASE_WORKFLOW"

broken="$WF_CODE_DIR/release.missing-region-guard.yml"
awk '
  $0 ~ /^          : "\$\{AWS_REGION:\?signing session is missing AWS_REGION\}"$/ { skip = 1 }
  skip {
    if ($0 ~ /^          fi$/) skip = 0
    next
  }
  { print }
' "$RELEASE_WORKFLOW" > "$broken"
expect_sign_step_mismatch 'missing-region-guard' "$broken"
expect_sign_step_match 'restore-after-missing-region-guard' "$RELEASE_WORKFLOW"

region_guard="$WF_CODE_DIR/region-guard.sh"
awk '
  $0 == ": \"${AWS_REGION:?signing session is missing AWS_REGION}\"" { copying = 1 }
  copying { print }
  copying && $0 == "fi" { exit }
' "$EXPECTED_SIGN_STEP" > "$region_guard"
guard_lines="$(awk 'END { print NR + 0 }' "$region_guard")"
[ "$guard_lines" -eq 6 ] \
  || fail "reviewed signing step is missing the six-line region fail-closed guard; found $guard_lines lines"

if AWS_REGION= AWS_DEFAULT_REGION= bash -eu -o pipefail "$region_guard"; then
  fail 'empty AWS_REGION/AWS_DEFAULT_REGION must fail closed'
fi
if AWS_REGION=ci-region-a AWS_DEFAULT_REGION=ci-region-b bash -eu -o pipefail "$region_guard"; then
  fail 'mismatched AWS_REGION/AWS_DEFAULT_REGION must fail closed'
fi
AWS_REGION=ci-region AWS_DEFAULT_REGION=ci-region bash -eu -o pipefail "$region_guard" \
  || fail 'restored identical AWS_REGION/AWS_DEFAULT_REGION must pass'

printf 'installer security tests passed\n'
