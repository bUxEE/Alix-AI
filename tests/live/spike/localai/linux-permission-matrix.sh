#!/usr/bin/env bash
# Capability matrix with Linux file-permission semantics, for hardening.md section 8 item 3. Docker Desktop's Windows bind
# mounts present every file as root-owned and world-writable, so the compose experiments cannot show what the three added
# capabilities (CHOWN, DAC_OVERRIDE, FOWNER) are for on a native Linux host, where ./data is owned by the invoking user.
# Here the directories live in the Docker VM's own ext4 (a throw-away named volume), owned by uid 1000 as they would be on a
# Linux host, and one container per capability set tries the file operations the guard and LocalAI's backend unpack do.
#
#   linux-permission-matrix.sh        prints the matrix, one line per (capability set, operation)
set -uo pipefail
docker() { MSYS_NO_PATHCONV=1 command docker "$@"; }
IMG="$(docker inspect alixai-localai --format '{{.Config.Image}}' 2>/dev/null || true)"
IMG="${IMG:-docker.io/localai/localai:v4.10.0@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b}"
VOL=alixai-m013-permmatrix
docker volume rm -f "$VOL" >/dev/null 2>&1
docker volume create "$VOL" >/dev/null
# Fixtures, rebuilt before EVERY probe so one probe's chmod, chown or new file cannot change what the next one sees: a
# directory owned by 1000:1000 mode 0755 (what ./data/models looks like for the invoking user on Linux) with a 0600 file owned
# by 1000, and a root-owned directory for contrast.
prep() {
  docker run --rm -v "$VOL:/d" --entrypoint sh "$IMG" -c '
    rm -rf /d/owned1000 /d/rootowned && mkdir -p /d/owned1000 /d/rootowned && chown 1000:1000 /d/owned1000 && chmod 0755 /d/owned1000 &&
    touch /d/owned1000/existing && chown 1000:1000 /d/owned1000/existing && chmod 0600 /d/owned1000/existing &&
    touch /d/rootowned/target' || exit 1
}
probe() { # label, docker flags...
  local label="$1"; shift
  prep
  docker run --rm "$@" -v "$VOL:/d" --entrypoint sh "$IMG" -c '
    r() { if "$@" 2>/dev/null; then echo ok; else echo DENIED; fi; }
    echo "write-new-file-in-dir-owned-by-1000: $(r touch /d/owned1000/new)"
    echo "write-new-file-in-root-owned-dir:    $(r touch /d/rootowned/new)"
    echo "read-0600-file-owned-by-1000:        $(r cat /d/owned1000/existing)"
    echo "chmod-file-owned-by-1000:            $(r chmod 0644 /d/owned1000/existing)"
    echo "chown-file-to-uid-2000:              $(r chown 2000 /d/rootowned/target)"
    echo "id: $(id -u) CapEff: $(grep CapEff /proc/self/status | cut -f2)"' | sed "s/^/[$label] /"
}
probe "ALL dropped, nothing added" --cap-drop ALL --security-opt no-new-privileges:true
probe "DAC_OVERRIDE only"          --cap-drop ALL --cap-add DAC_OVERRIDE --security-opt no-new-privileges:true
probe "FOWNER only"                --cap-drop ALL --cap-add FOWNER --security-opt no-new-privileges:true
probe "CHOWN only"                 --cap-drop ALL --cap-add CHOWN --security-opt no-new-privileges:true
probe "shipped: CHOWN+DAC_OVERRIDE+FOWNER" --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --security-opt no-new-privileges:true
docker volume rm -f "$VOL" >/dev/null 2>&1
