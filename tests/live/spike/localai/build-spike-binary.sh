#!/usr/bin/env bash
# Builds `alixai` with ONE registry entry changed, for the end-to-end plumbing request only.
#
#   build-spike-binary.sh <registry-id> <out.exe> [runtime-version]
#
# Why this exists: M0-13 rejected every controller model against the pass bar in docs/evidence/m0-spike/report.md, and a rejected
# entry is not activatable (models.Verify refuses it, `alixai serve` keeps the inference path off). The M0 exit gate still asks
# for one text request through AlixAI and LocalAI, which is a plumbing proof and not a qualification. The shipped binary and the
# shipped registry are left exactly as the verdict wrote them; this script builds a SEPARATE spike-only binary whose embedded copy
# of that one entry is changed by `go build -overlay` (the overlay reaches //go:embed files; nothing in models/registry/ is edited):
#
#   status                         rejected -> candidate    (candidate = evaluation re-opened; Verify accepts it)
#   backend.allowed_runtime_versions  []   -> [<runtime-version>]   (default v4.10.0, the pinned LocalAI)
#   fallback.model_id              <id>    -> null          (a rejected fallback would make the loader refuse the registry)
#
# The entry's sha256, size and file name are untouched, so serve still hashes the model file against the registry pin.
# <out.exe>.variant.json records the exact diff and is copied into e2e.json, so the evidence never claims the shipped registry
# supports what the variant does.
set -euo pipefail
id="${1:?registry id}"; out="${2:?output binary}"; version="${3:-v4.10.0}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../../.." && pwd)"
export PATH="/c/Program Files/Go/bin:$PATH"
src="$ROOT/models/registry/$id.yaml"
[ -f "$src" ] || { echo "no registry entry $src" >&2; exit 2; }
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
variant="$work/$id.yaml"

# Only these three lines may change. Each sed is anchored so a note that mentions the word cannot match.
sed -e 's/^status: rejected$/status: candidate/' \
    -e "s/^  allowed_runtime_versions: \[\]\$/  allowed_runtime_versions: [\"$version\"]/" \
    -e 's/^  model_id: [a-z0-9][a-z0-9._-]*$/  model_id: null/' "$src" > "$variant"
changed="$(diff "$src" "$variant" | grep -c '^>' || true)"
[ "$changed" -ge 2 ] || { echo "the variant changed $changed line(s); expected status and allowed_runtime_versions at least" >&2; exit 3; }

winroot="$(cd "$ROOT" && pwd -W 2>/dev/null || pwd)"
winvariant="$(cd "$work" && pwd -W 2>/dev/null || pwd)/$id.yaml"
printf '{"Replace": {"%s/models/registry/%s.yaml": "%s"}}\n' "$winroot" "$id" "$winvariant" > "$work/overlay.json"

mkdir -p "$(dirname "$out")"
( cd "$ROOT" && CGO_ENABLED=0 go build -overlay "$work/overlay.json" -o "$out" ./cmd/alixai )

{
  printf '{\n  "purpose": "plumbing proof only: the shipped registry marks this model rejected, so the shipped binary refuses to run it",\n'
  printf '  "base_entry": "models/registry/%s.yaml",\n  "mechanism": "go build -overlay replaces the embedded copy of that one file at build time; no file in the repository was edited",\n' "$id"
  printf '  "runtime_version": "%s",\n  "diff_base_to_variant": [\n' "$version"
  { diff "$src" "$variant" || true; } | grep '^[<>]' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/^/    "/' -e 's/$/",/' | sed '$ s/,$//'
  printf '  ]\n}\n'
} > "$out.variant.json"
echo "built $out ($changed changed line(s)); variant recorded in $out.variant.json"
