#!/bin/bash
# AlixAI guard in front of the optional CLM sidecar (vLLM encoder + CLM service), one script for both services.
#
#   entrypoint.sh vllm    verify the frozen Qwen3-8B encoder, then exec `vllm serve`
#   entrypoint.sh clm     verify the CLM head, the wheel and the encoder pin, then exec the CLM service
#
# Runs inside the container before anything is served. Fail closed: every check that fails stops the container
# with exit 78 (EX_CONFIG) and nothing is served. The model registry is the authority for revisions and hashes:
#
#   1. the registry entry (models/registry/<id>.yaml, mounted read-only at /registry) must exist, must not be
#      rejected or retired, and its artifact.revision and artifact.sha256 must equal the line config/pins.txt
#      holds for the file the entry names in artifact.filename. A registry entry that disagrees with the pins,
#      or that is missing, refuses the start.
#   2. every file in pins.txt for the role must exist in the model directory with exactly the pinned sha256, and
#      the directory must hold nothing else (vLLM would load any extra safetensors it found; a symlink is refused).
#
# The only network use is the optional fetch (ALIXAI_FETCH_MODELS=true, compose.fetch.yaml): it downloads missing
# files from the exact pinned urls (https, huggingface.co or files.pythonhosted.org only) into a .part file and
# renames it only after the sha256 matched. Without that switch nothing is downloaded and no network exists.
# This script runs no user-supplied commands and never builds a command line from environment text.
set -euo pipefail

ROLE="${1:-}"
PINS="${ALIXAI_PINS_FILE:-/alixai/pins.txt}"
REGISTRY_DIR="${ALIXAI_REGISTRY_DIR:-/registry}"
ENCODER_DIR="${ALIXAI_ENCODER_DIR:-/models/encoder}"
CLM_DIR="${ALIXAI_CLM_DIR:-/models/clm}"
CLM_ID="${ALIXAI_CLM_REGISTRY_ID:-clm-v0.1-8b}"
ENCODER_ID="${ALIXAI_ENCODER_REGISTRY_ID:-qwen3-8b}"

die() {
  echo "alixai-clm: refusing to start: $*" >&2
  exit 78
}
note() { echo "alixai-clm: $*" >&2; }

case "$ROLE" in
  vllm | clm) ;;
  *) die "usage: entrypoint.sh vllm|clm (got '${ROLE}')" ;;
esac
for tool in awk sha256sum find python3; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is missing from the image"
done
[ -r "$PINS" ] || die "$PINS is not readable"

# ids that name a registry file: lower case, digits, dot, dash, underscore; no path separators
for id in "$CLM_ID" "$ENCODER_ID"; do
  case "$id" in
    "" | *[!a-z0-9._-]* | .* ) die "registry id '$id' is not a plain id (lower case, digits, . _ -)" ;;
  esac
done

# --- pins.txt access ---------------------------------------------------------------------------
# prints "role model revision sha256 path url" lines of the given roles
pin_lines() { awk -v rs=" $* " '$0 !~ /^[[:space:]]*#/ && NF == 6 && index(rs, " " $1 " ") > 0 { print }' "$PINS"; }

# --- registry cross-check ----------------------------------------------------------------------
reg_top() { awk -v k="$2" '$0 ~ "^" k ":" { sub(/^[^:]*:[ \t]*/, ""); gsub(/[\r"'"'"' \t]/, ""); print; exit }' "$1"; }
reg_art() {
  awk -v k="$2" '
    /^[A-Za-z_]/ { inblk = ($0 ~ /^artifact:/); next }
    inblk && $0 ~ "^  " k ":" { sub(/^[^:]*:[ \t]*/, ""); gsub(/[\r"'"'"' \t]/, ""); print; exit }' "$1"
}

# check_registry <registry-id> <pins-role> <pins-model-id>
check_registry() {
  local id="$1" prole="$2" pmodel="$3" file status rev fname sha pin prev psha
  file="$REGISTRY_DIR/$id.yaml"
  [ -r "$file" ] || die "model registry entry $file not found (the registry must carry '$id'; set ALIXAI_*_REGISTRY_ID if it is named differently)"
  status="$(reg_top "$file" status)"
  case "$status" in
    rejected | retired) die "registry entry '$id' has status '$status'" ;;
    "") die "registry entry '$id' has no status" ;;
  esac
  rev="$(reg_art "$file" revision)"
  fname="$(reg_art "$file" filename)"
  sha="$(reg_art "$file" sha256)"
  for v in "$rev" "$fname" "$sha"; do
    case "$v" in "" | null | ~) die "registry entry '$id' does not pin artifact.revision, artifact.filename and artifact.sha256 (unpinned)" ;; esac
  done
  pin="$(awk -v r="$prole" -v m="$pmodel" -v f="$fname" '$0 !~ /^[[:space:]]*#/ && NF == 6 && $1 == r && $2 == m && $5 == f { print $3, $4; exit }' "$PINS")"
  [ -n "$pin" ] || die "registry entry '$id' names artifact.filename '$fname', which config/pins.txt does not pin for $prole $pmodel"
  prev="${pin% *}"
  psha="${pin#* }"
  [ "$rev" = "$prev" ] || die "registry revision of '$id' ($rev) differs from the pinned revision ($prev)"
  [ "$sha" = "$psha" ] || die "registry sha256 of '$id' ($sha) differs from the pinned sha256 ($psha)"
  note "registry entry '$id' ($status) matches the pinned revision $prev"
}

# --- optional fetch (compose.fetch.yaml only) ---------------------------------------------------
fetch_missing() { # fetch_missing <pins-role> <dir>
  [ "${ALIXAI_FETCH_MODELS:-false}" = "true" ] || return 0
  [ -d "$2" ] && [ -w "$2" ] || die "$2 is not a writable directory (create it under ./data first)"
  python3 - "$1" "$2" "$PINS" <<'PY' || die "fetch failed (see above); the host-side fetch.sh is the fallback"
import hashlib, os, sys, urllib.parse, urllib.request

role, dest, pins = sys.argv[1:4]
ALLOWED = {"huggingface.co", "files.pythonhosted.org"}
for raw in open(pins, encoding="utf-8"):
    f = raw.split()
    if len(f) != 6 or raw.lstrip().startswith("#") or f[0] != role:
        continue
    _, model, rev, sha, rel, url = f
    target = os.path.join(dest, rel)
    if os.path.exists(target):
        continue
    u = urllib.parse.urlparse(url)
    if u.scheme != "https" or u.hostname not in ALLOWED or ".." in rel or rel.startswith("/"):
        sys.exit("pinned url or path is not allowed: " + rel)
    part = target + ".part"
    h = hashlib.sha256()
    print("alixai-clm: fetching " + rel, file=sys.stderr, flush=True)
    with urllib.request.urlopen(url, timeout=60) as r, open(part, "wb") as out:
        while True:
            b = r.read(1 << 20)
            if not b:
                break
            h.update(b)
            out.write(b)
    if h.hexdigest() != sha:
        os.remove(part)
        sys.exit("sha256 mismatch for " + rel + ": got " + h.hexdigest())
    os.replace(part, target)
PY
}

# --- verify a model directory against pins.txt ----------------------------------------------------
verify_dir() { # verify_dir <dir> <pins-role>...
  local dir="$1" line rel sha got count=0 expect
  shift
  [ -d "$dir" ] || die "$dir is not a directory (run fetch.sh on the host, or use compose.fetch.yaml; see README.md)"
  if [ -n "$(find "$dir" -type l -print -quit)" ]; then die "$dir holds a symlink; refused"; fi
  # nothing but the pinned files (a .part file from an interrupted fetch counts as unexpected)
  expect="$(pin_lines "$@" | awk '{ print $5 }' | sort)"
  [ -n "$expect" ] || die "pins.txt has no $* lines"
  got="$(cd "$dir" && find . -type f | sed 's|^\./||' | sort)"
  [ -n "$got" ] || die "$dir is empty (run fetch.sh on the host, or use compose.fetch.yaml; see README.md)"
  if [ "$got" != "$expect" ]; then
    die "$dir does not hold exactly the pinned files (missing or unexpected: $(printf '%s
%s
' "$expect" "$got" | sort | uniq -u | tr '
' ' '))"
  fi
  while read -r _ _ _ sha rel _; do
    note "hashing $rel"
    got="$(sha256sum "$dir/$rel" | awk '{ print $1 }')"
    [ "$got" = "$sha" ] || die "sha256 of $dir/$rel is $got, pinned $sha"
    count=$((count + 1))
  done < <(pin_lines "$@")
  note "$count file(s) of $* verified against pins.txt"
}

# --- roles ------------------------------------------------------------------------------------------
case "$ROLE" in
  vllm)
    check_registry "$ENCODER_ID" encoder qwen3-8b
    fetch_missing encoder "$ENCODER_DIR"
    verify_dir "$ENCODER_DIR" encoder
    # CPU only, no accelerator and no telemetry are set by compose.yaml; a leftover GPU selector is refused
    # here as well so a copied environment cannot hand the encoder an AMD or other GPU.
    for v in HIP_VISIBLE_DEVICES ROCR_VISIBLE_DEVICES CUDA_VISIBLE_DEVICES; do
      val="${!v:-}"
      case "$val" in "" | -1) ;; *) die "$v='$val': this sidecar runs on CPU only" ;; esac
    done
    [ "${VLLM_NO_USAGE_STATS:-}" = "1" ] || die "VLLM_NO_USAGE_STATS must be 1 (telemetry stays off)"
    exec vllm serve "$ENCODER_DIR" \
      --served-model-name qwen3-8b \
      --runner pooling \
      --dtype bfloat16 \
      --max-model-len 2048 \
      --enable-prefix-caching \
      --disable-log-stats \
      --host 0.0.0.0 \
      --port 8090
    ;;
  clm)
    # --- API key: required, single, not a placeholder -------------------------------------------
    key="${CLM_API_KEY:-}"
    if [ -z "$key" ]; then
      [ "${ALIXAI_CLM_ALLOW_NO_KEY:-false}" = "true" ] || die "CLM_API_KEY is empty (the adapters/decision/clm client sends api_key_ref as a Bearer key: set a key, or ALIXAI_CLM_ALLOW_NO_KEY=true only for a manual loopback-only run, see README.md)"
      note "WARNING: no API key; the loopback-only publish is the only barrier"
    else
      case "$key" in
        *[[:space:]]*) die "CLM_API_KEY must not contain whitespace" ;;
        change-me* | changeme*) die "CLM_API_KEY is still the .env.example placeholder" ;;
      esac
      [ "${#key}" -ge 32 ] || die "CLM_API_KEY must be at least 32 characters (openssl rand -hex 32)"
    fi
    unset key
    check_registry "$CLM_ID" head clm-v0.1-8b
    check_registry "$ENCODER_ID" encoder qwen3-8b
    fetch_missing head "$CLM_DIR"
    fetch_missing wheel "$CLM_DIR"
    verify_dir "$CLM_DIR" head wheel
    # the wheel is pure Python and brings its own dependency list, which is NOT installed: torch, fastapi,
    # uvicorn, numpy and requests come from the pinned image. No pip, no index, no network.
    SITE="${TMPDIR:-/tmp}/clm-site"
    mkdir -p "$SITE"
    python3 -m zipfile -e "$CLM_DIR/contrastive_lm-0.1.0-py3-none-any.whl" "$SITE"
    export PYTHONPATH="$SITE"
    export HF_HUB_OFFLINE=1 HF_HUB_DISABLE_TELEMETRY=1 DO_NOT_TRACK=1
    # --no-download: without --ckpt the service would fetch the head, and its download helper also sends a
    # HEAD request to the Hub to feed the download counter. --no-ui drops the playground; --cors stays off.
    exec python3 -c 'from clm.server import main; main()' \
      --host 0.0.0.0 \
      --port 8700 \
      --emb-url "http://vllm:8090/v1/embeddings" \
      --emb-model qwen3-8b \
      --max-tokens 2048 \
      --ckpt "$CLM_DIR/CLM_v0.1-8B.pt" \
      --no-download \
      --no-ui \
      --device cpu \
      --action-cache "${CLM_ACTION_CACHE:-256MiB}"
    ;;
esac
