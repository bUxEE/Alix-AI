#!/bin/sh
# Reproduces the emulated 4 GiB connected-mode footprint run of task X-23 (docs/evidence/x-benchmarks/).
# Run from the repository root with Go and Docker (or Podman, DOCKER=podman) installed. Nothing is pulled:
# IMAGE must already be local (any image works; the binaries are bind-mounted and the entrypoint is replaced).
#
# The container is capped like deploy/container/localai/compose.4g.yaml (4 GiB, no swap, 4 CPUs), has no
# network at all (only loopback inside it), is not privileged, and is removed when it exits.
set -eu

DOCKER=${DOCKER:-docker}
IMAGE=${IMAGE:-alixai:verify}
OUT=${OUT:-docs/evidence/x-benchmarks}
RESULTS=${RESULTS:-benchmarks/results/x}
BIN=$(mktemp -d)
chmod 755 "$BIN"
trap 'rm -rf "$BIN"' EXIT

export MSYS_NO_PATHCONV=1 # Git Bash on Windows: do not rewrite the container paths below
# Native Windows tools (go, docker) need Windows paths; elsewhere the paths are used as they are.
HOSTBIN=$BIN
HOSTROOT=$(pwd)
if command -v cygpath >/dev/null 2>&1; then HOSTBIN=$(cygpath -m "$BIN"); HOSTROOT=$(cygpath -m "$(pwd)"); fi
SHA=$(git rev-parse HEAD)
DIRTY=""
if [ -n "$(git status --porcelain -- tools cmd internal adapters pkg tests/performance)" ]; then DIRTY="--git-dirty"; fi

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$HOSTBIN/providercompare" ./tools/providercompare
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$HOSTBIN/alixai" ./cmd/alixai
mkdir -p "$BIN/out" "$RESULTS" "$OUT"

$DOCKER run --rm --name x23-footprint \
  --memory 4g --memory-swap 4g --cpus 4 --network none \
  --tmpfs /work:rw,size=512m,mode=1777 \
  -v "$HOSTBIN:/app:ro" -v "$HOSTBIN/out:/out" -v "$HOSTROOT/tests/performance:/repo/tests/performance:ro" \
  --entrypoint /app/providercompare "$IMAGE" \
  footprint --root /repo --result-dir /out/res --evidence /out/footprint-4g.json \
  --core-bin /app/alixai --workdir /work --git-sha "$SHA" $DIRTY

cp "$BIN"/out/res/*.json "$RESULTS"/
cp "$BIN/out/footprint-4g.json" "$OUT/footprint-4g.json"
go run ./tools/bench check "$RESULTS"
