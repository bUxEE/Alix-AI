#!/usr/bin/env bash
# Host-side fetch of the spike's model files into deploy/container/localai/data/models (git-ignored bind mount).
#
# Why on the host: on the M0-13 dev box Avast re-signs TLS and the container does not trust its root, so LocalAI's own
# preload fails with "x509: certificate signed by unknown authority" (evidence/m0-spike/hardening-live.md). The host
# trusts it, so curl verifies TLS normally here. Integrity is unchanged: each file's sha256 must equal the pin in
# models.yaml (the registry's value), and LocalAI's preload re-verifies it at every start.
#
#   fetch-models.sh [filename-substring ...]      (default: every file in models.yaml)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST="$(cd "$HERE/../../../../deploy/container/localai" && pwd)/data/models"
mkdir -p "$DEST"
# filename|uri|sha256, from models.yaml (download_files entries)
mapfile -t rows < <(awk '
  /^ *- filename:/ {f=$3}
  /^ *uri:/ {u=$2}
  /^ *sha256:/ {print f "|" u "|" $2}' "$HERE/models.yaml" | sort -u)
for row in "${rows[@]}"; do
  IFS='|' read -r name uri sha <<<"$row"
  if [ "$#" -gt 0 ]; then
    keep=0; for pat in "$@"; do case "$name" in *"$pat"*) keep=1 ;; esac; done
    [ "$keep" = 1 ] || continue
  fi
  if [ -f "$DEST/$name" ] && [ "$(sha256sum "$DEST/$name" | cut -d' ' -f1)" = "$sha" ]; then
    echo "ok      $name (already present, sha256 matches)"; continue
  fi
  echo "fetch   $name"
  curl --fail --location --retry 5 --retry-delay 5 --continue-at - --silent --show-error -o "$DEST/$name.part" "$uri"
  got="$(sha256sum "$DEST/$name.part" | cut -d' ' -f1)"
  if [ "$got" != "$sha" ]; then
    echo "MISMATCH $name: got $got, pinned $sha" >&2
    mv "$DEST/$name.part" "$DEST/$name.mismatch"
    exit 1
  fi
  mv "$DEST/$name.part" "$DEST/$name"
  echo "ok      $name sha256 $got"
done
