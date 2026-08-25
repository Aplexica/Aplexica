#!/usr/bin/env bash
# Pack aplexica-$VERSION-source.tar.gz with GNU tar.
# git archive and BSD tar emit a root pax_global_header that fails
# releaseassetcheck's versioned-prefix contract. GNU tar --format=gnu does not.
set -euo pipefail

usage() {
  printf 'usage: %s --version VERSION --output PATH [--epoch EPOCH] [--repo DIR]\n' "$0" >&2
  exit 2
}

version=""
output=""
epoch="${SOURCE_DATE_EPOCH:-}"
repo="."

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || usage
      version="$2"
      shift 2
      ;;
    --output)
      [ "$#" -ge 2 ] || usage
      output="$2"
      shift 2
      ;;
    --epoch)
      [ "$#" -ge 2 ] || usage
      epoch="$2"
      shift 2
      ;;
    --repo)
      [ "$#" -ge 2 ] || usage
      repo="$2"
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

[ -n "$version" ] && [ -n "$output" ] || usage
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] \
  || { printf 'version must be bare MAJOR.MINOR.PATCH\n' >&2; exit 1; }

resolve_gnu_tar() {
  local candidate
  for candidate in gtar tar; do
    if command -v "$candidate" >/dev/null 2>&1 \
      && "$candidate" --version 2>/dev/null | grep -Fq 'GNU tar'; then
      command -v "$candidate"
      return 0
    fi
  done
  printf 'GNU tar (gtar or tar) is required to pack the source archive\n' >&2
  return 1
}

gnu_tar="$(resolve_gnu_tar)"
prefix="aplexica-${version}"
if [ -z "$epoch" ]; then
  epoch="$(git -C "$repo" log -1 --format=%ct HEAD)"
fi
[[ "$epoch" =~ ^[0-9]+$ ]] || { printf 'invalid SOURCE_DATE_EPOCH\n' >&2; exit 1; }
test -d "$repo"
test -f "$repo/.git" -o -d "$repo/.git"

case "$output" in
  /*) ;;
  *) output="$(pwd)/$output" ;;
esac
mkdir -p "$(dirname "$output")"
git -C "$repo" ls-files -z | "$gnu_tar" \
  -C "$repo" \
  --null \
  --sort=name --format=gnu \
  --mtime="@${epoch}" \
  --owner=0 --group=0 --numeric-owner \
  --mode='u+rwX,go+rX,go-w' \
  --no-acls --no-xattrs --no-selinux \
  --transform="s,^,${prefix}/," \
  --use-compress-program='gzip -n' \
  -cf "$output" \
  --files-from=-
test -s "$output"

python3 - "$output" "$prefix" <<'PY'
import gzip
import sys

path, prefix = sys.argv[1], sys.argv[2]
with gzip.open(path, "rb") as handle:
    block = handle.read(512)
if len(block) < 512:
    raise SystemExit("source archive is too short")
name = block[0:100].split(b"\0", 1)[0].decode("ascii", "replace")
typeflag = chr(block[156])
if typeflag in "gx" or name in {"pax_global_header", "PaxHeaders.0", "././@PaxHeader"}:
    raise SystemExit("source member %r is a pax header" % name)
if name != prefix and not name.startswith(prefix + "/"):
    raise SystemExit("source member %r is outside prefix %s/" % (name, prefix))
PY
