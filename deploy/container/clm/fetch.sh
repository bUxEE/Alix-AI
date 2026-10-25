#!/usr/bin/env bash
# Host-side fetch of the pinned CLM sidecar files into ./data (the working path where the container's TLS does not
# trust the network's certificate authority; README.md "Fetching on the host"). Not run by `plan verify`.
#
#   ./fetch.sh [encoder|clm|all]      (default: all)
#
# Reads config/pins.txt, downloads only the exact pinned urls (https, huggingface.co and files.pythonhosted.org),
# resumes an interrupted .part file, and renames a file into place only after its sha256 matched. An existing file
# with a wrong hash is never replaced: delete it yourself. ~16.4 GB for the encoder, ~76 MB for the head and wheel.
# Needs curl, sha256sum and awk on the host; no Docker, no pip, no Python.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

WHAT="${1:-all}"
case "$WHAT" in
  encoder) ROLES="encoder" ;;
  clm) ROLES="head wheel" ;;
  all) ROLES="encoder head wheel" ;;
  *) echo "usage: $0 [encoder|clm|all]" >&2; exit 2 ;;
esac

fail() { echo "FETCH FAIL: $*" >&2; exit 1; }

for tool in curl sha256sum awk; do command -v "$tool" >/dev/null 2>&1 || fail "$tool is missing"; done

dir_of() { case "$1" in encoder) echo data/encoder ;; *) echo data/clm ;; esac; }
mkdir -p data/encoder data/clm

for role in $ROLES; do
  dir="$(dir_of "$role")"
  while read -r r _ rev sha rel url; do
    [ "$r" = "$role" ] || continue
    case "$url" in
      https://huggingface.co/* | https://files.pythonhosted.org/*) ;;
      *) fail "pinned url is not allowed: $url" ;;
    esac
    case "$rel" in */* | .* | "") fail "pinned path is not a plain file name: $rel" ;; esac
    target="$dir/$rel"
    if [ -f "$target" ]; then
      got="$(sha256sum "$target" | awk '{ print $1 }')"
      [ "$got" = "$sha" ] || fail "$target exists with sha256 $got, pinned $sha; delete it to fetch again"
      echo "have   $target"
      continue
    fi
    echo "fetch  $target (revision ${rev:0:12})"
    curl -fL --progress-bar --retry 5 --retry-delay 3 -C - -o "$target.part" "$url"
    got="$(sha256sum "$target.part" | awk '{ print $1 }')"
    if [ "$got" != "$sha" ]; then
      rm -f "$target.part"
      fail "sha256 of $rel is $got, pinned $sha"
    fi
    mv "$target.part" "$target"
  done < <(awk '$0 !~ /^[[:space:]]*#/ && NF == 6' config/pins.txt)
done
echo "FETCH OK ($WHAT): every file matches config/pins.txt"
