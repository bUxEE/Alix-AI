#!/bin/bash
# AlixAI guard in front of the LocalAI image's own /entrypoint.sh.
#
# Runs inside the container before LocalAI starts. It only checks and then execs; it never
# downloads anything and runs no user-supplied commands. Models and backends are fetched by
# LocalAI itself (see config/models.yaml and config/backend-gallery.yaml).
#
# Fail closed: every check that fails stops the container with exit 78 (EX_CONFIG).
#
# Two switches exist, both explicit, both default off, neither a LocalAI setting:
#   ALIXAI_MODELS_READONLY=true     /models is mounted read-only (compose.readonly-models.yaml)
#   ALIXAI_ALLOW_VERBOSE_LOG=true   LocalAI may log at debug or trace (development only)
set -euo pipefail

die() {
  echo "alixai-localai: refusing to start: $*" >&2
  exit 78
}

# --- API key: required, single, not a placeholder -------------------------------------------
key="${LOCALAI_API_KEY:-}"
[ -n "$key" ] || die "LOCALAI_API_KEY is empty"
case "$key" in
  *[[:space:]]*) die "LOCALAI_API_KEY must not contain whitespace" ;;
  *,*) die "LOCALAI_API_KEY must be a single key (LocalAI treats commas as a key list)" ;;
  change-me* | changeme*) die "LOCALAI_API_KEY is still the .env.example placeholder" ;;
esac
[ "${#key}" -ge 32 ] || die "LOCALAI_API_KEY must be at least 32 characters (openssl rand -hex 32)"
unset key

# --- data directories: the bind mounts must exist and be writable --------------------------
# /models alone may be read-only, and only on request: a read-only models directory means a model file whose
# hash differs from its pin cannot be deleted and replaced at start (LocalAI does that, hardening-live.md 2b),
# and a compromised runtime cannot rewrite a model (HC-24). It is a switch, not an inference from the mount:
# a /models that happens to be read-only without it is a mistake and still refuses to start.
models="${LOCALAI_MODELS_PATH:-/models}"
case "${ALIXAI_MODELS_READONLY:-false}" in
  true)
    [ -d "$models" ] || die "$models is not a directory (ALIXAI_MODELS_READONLY=true needs the models provisioned under ./data/models first)"
    if [ -w "$models" ]; then
      die "ALIXAI_MODELS_READONLY=true but $models is writable; mount ./data/models with :ro (compose.readonly-models.yaml does)"
    fi
    ;;
  false | "")
    if [ ! -d "$models" ] || [ ! -w "$models" ]; then
      die "$models is not a writable directory (create the host dir under ./data first; on Linux it must be writable by the container user, see README Troubleshooting; a deliberately read-only models directory needs ALIXAI_MODELS_READONLY=true)"
    fi
    ;;
  *) die "ALIXAI_MODELS_READONLY must be true or false" ;;
esac
for dir in "${LOCALAI_BACKENDS_PATH:-/backends}" "${LOCALAI_DATA_PATH:-/data}"; do
  if [ ! -d "$dir" ] || [ ! -w "$dir" ]; then
    die "$dir is not a writable directory (create the host dir under ./data first; on Linux it must be writable by the container user, see README Troubleshooting)"
  fi
done

# --- config directory: must be read-only (nothing may add runtime settings or API keys) -----
conf="${LOCALAI_CONFIG_DIR:-/configuration}"
[ -d "$conf" ] || die "$conf is missing (mount ./config read-only)"
if [ -w "$conf" ]; then
  die "$conf is writable; mount ./config with :ro (LocalAI hot-applies runtime_settings.json, api_keys.json and external_backends.json from it)"
fi

# --- log level: info, never debug or trace ---------------------------------------------------
# At debug and trace LocalAI logs the model's completion in full (`DEBUG Response response="{...}"`; the
# prompt is never logged), and a completion can hold the user's own data, a proposal's field values or a
# refusal. The container log is not under AlixAI's redaction rules. Measured in M0-13 (hardening-live.md,
# HC-34). LOCALAI_DEBUG and DEBUG are the deprecated spellings of the same switch.
level="$(printf '%s' "${LOCALAI_LOG_LEVEL:-info}" | tr '[:upper:]' '[:lower:]')"
verbose=""
case "$level" in
  debug | trace) verbose="LOCALAI_LOG_LEVEL=$level" ;;
esac
case "$(printf '%s' "${LOCALAI_DEBUG:-}${DEBUG:-}" | tr '[:upper:]' '[:lower:]')" in
  "" | false | 0 | no | off) ;;
  *) verbose="${verbose:+$verbose and }LOCALAI_DEBUG/DEBUG" ;;
esac
if [ -n "$verbose" ]; then
  case "${ALIXAI_ALLOW_VERBOSE_LOG:-false}" in
    true) echo "alixai-localai: WARNING: verbose logging is on ($verbose): the container log now holds every model completion in full" >&2 ;;
    false | "") die "$verbose would put every model completion in the container log (HC-34); keep LOCALAI_LOG_LEVEL=info, or set ALIXAI_ALLOW_VERBOSE_LOG=true for a throw-away development run" ;;
    *) die "ALIXAI_ALLOW_VERBOSE_LOG must be true or false" ;;
  esac
fi

exec /entrypoint.sh "$@"
