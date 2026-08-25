#!/usr/bin/env bash
# Fetch Aplexica/aplexica-portal at an exact commit with PORTAL_FETCH_TOKEN.
# Never prints the token. Does not execute Portal code.
set -euo pipefail

usage() {
  printf 'usage: %s --repo OWNER/NAME --commit SHA --output-dir DIR\n' "$0" >&2
  exit 2
}

repo=""
commit=""
output=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --repo)
      [ "$#" -ge 2 ] || usage
      repo="$2"
      shift 2
      ;;
    --commit)
      [ "$#" -ge 2 ] || usage
      commit="$2"
      shift 2
      ;;
    --output-dir)
      [ "$#" -ge 2 ] || usage
      output="$2"
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

[ -n "$repo" ] && [ -n "$commit" ] && [ -n "$output" ] || usage
[ "$repo" = "Aplexica/aplexica-portal" ] || { printf 'repo must be Aplexica/aplexica-portal\n' >&2; exit 1; }
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { printf 'commit must be 40 lowercase hex\n' >&2; exit 1; }
test -n "${PORTAL_FETCH_TOKEN:-}" || { printf 'PORTAL_FETCH_TOKEN is required\n' >&2; exit 1; }

mkdir -p "$output"
git -C "$output" init --quiet
GIT_TERMINAL_PROMPT=0 git -C "$output" \
  -c http.extraheader="AUTHORIZATION: basic $(printf 'x-access-token:%s' "$PORTAL_FETCH_TOKEN" | base64 | tr -d '\n')" \
  fetch --depth=1 "https://github.com/${repo}.git" "$commit"
git -C "$output" checkout --detach FETCH_HEAD
test "$(git -C "$output" rev-parse HEAD)" = "$commit"
git -C "$output" ls-tree -r -t HEAD > "$output/.portal-tree.txt"
test -s "$output/.portal-tree.txt"
awk '
  $1 == "040000" && $2 == "tree" { next }
  $1 == "100644" && $2 == "blob" { next }
  $1 == "100755" && $2 == "blob" { next }
  { print; bad = 1 }
  END { exit bad + 0 }
' "$output/.portal-tree.txt"
rm -f "$output/.portal-tree.txt"
