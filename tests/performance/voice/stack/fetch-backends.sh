#!/usr/bin/env bash
# Host-side install of the two voice backends of LocalAI v4.10.0 into a gitignored data directory (M4-06).
# The container cannot pull them (a TLS-intercepting security product), so the host's daemon pulls the digest-pinned
# image and the layers are unpacked in a Linux container so symlinks stay symlinks. Same recipe as
# deploy/container/localai/README.md "Backend and model pull from the host"; nothing under deploy/ is edited.
#
#   fetch-backends.sh [data dir]      (default: benchmarks/tmp/voice-data, ignored by git)
set -euo pipefail
export PATH="/c/Program Files/Docker/Docker/resources/bin:$PATH"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../../.." && pwd)"
DATA="${1:-$ROOT/benchmarks/tmp/voice-data}"
mkdir -p "$DATA/backends" "$DATA/models" "$DATA/state"
WDATA="$(cd "$DATA" && pwd -W 2>/dev/null || pwd)"

# name|alias|digest of the multi-arch index of quay.io/go-skynet/local-ai-backends (resolved 2026-10-04 from the v4.10.0 tags:
# v4.10.0-cpu-whisper and v4.10.0-piper (no cpu- prefix upstream). VAD needs no backend of its own: LocalAI v4.10.0 serves the
# ggml Silero model through the whisper backend with the vad_only option (gallery entry silero-vad-ggml).
BACKENDS=(
  "whisper|whisper|sha256:cd3d42dbc15b7a6cf29a1e3c8509bd7a29abc16cce766178349de66898bd1475"
  "piper|piper|sha256:d02327c0f851d3858724d3ee5d977241e7c617a0ed575eca58b28e57ec10792a"
  # the English-only Kokoro challenger (Rust, ONNX; protocol stop rule: measured because a Piper voice failed a gate), v4.10.0-cpu-kokoros
  "kokoros|kokoros|sha256:613c05d872ad1e1b24b4cbbd981cef4f80c6e1369221c54c8ca23bb0a0872d7b"
)
for b in "${BACKENDS[@]}"; do
  IFS='|' read -r name alias digest <<<"$b"
  uri="quay.io/go-skynet/local-ai-backends@${digest}"
  if [ -f "$DATA/backends/$name/metadata.json" ] && grep -q "$digest" "$DATA/backends/$name/metadata.json"; then
    echo "backend $name already installed at $digest"; continue
  fi
  rm -rf "$DATA/backends/$name"; mkdir -p "$DATA/backends/$name"
  docker pull "$uri" >/dev/null
  cid="alixai-voice-extract-$name"
  docker rm -f "$cid" >/dev/null 2>&1 || true
  docker create --name "$cid" "$uri" true >/dev/null
  MSYS_NO_PATHCONV=1 docker export "$cid" | MSYS_NO_PATHCONV=1 docker run --rm -i -v "$WDATA/backends:/out" alpine:latest \
    tar -x -C "/out/$name" --exclude .dockerenv --exclude dev --exclude etc --exclude proc --exclude sys
  docker rm "$cid" >/dev/null
  printf '{"alias":"%s","name":"%s","gallery_url":"file:///backends/alixai-backend-gallery.yaml","installed_at":"%s","version":"v4.10.0","uri":"%s","digest":"%s"}\n' \
    "$alias" "$name" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$uri" "$digest" > "$DATA/backends/$name/metadata.json"
  echo "backend $name installed: $(du -sm "$DATA/backends/$name" | cut -f1) MB"
done
