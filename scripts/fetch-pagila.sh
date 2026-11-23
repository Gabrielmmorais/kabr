#!/usr/bin/env bash
# Download the Pagila sample database (schema + data) used by the Kabr demo.
#
# - Files come from a pinned upstream commit (never a moving branch).
# - Every file is verified against a hard-coded SHA-256 before it is kept.
# - Idempotent: files whose hash already matches are not downloaded again.
#
# Upstream: https://github.com/devrimgunduz/pagila (PostgreSQL License)
# Pinned:   tag pagila-v3.1.0, the last release that supports PostgreSQL 17
#           (v4.0.0+ requires PostgreSQL 18: uuidv7(), virtual generated columns).
set -euo pipefail

PAGILA_REPO="devrimgunduz/pagila"
PAGILA_COMMIT="fef9675714cfba1756df4719b5e36075a7ddf90e" # tag pagila-v3.1.0
PAGILA_SCHEMA_SHA256="8ce358e4c8014087b85296694a0893887bd7a4190e3ce407f2721b86b98e5707"
PAGILA_DATA_SHA256="fb81bec377687c83e11d2a24916ae28656d85550bf0ada798305bf7e2af9823b"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEST_DIR="${PAGILA_DEST_DIR:-${SCRIPT_DIR}/../deploy/compose/data}"
BASE_URL="https://raw.githubusercontent.com/${PAGILA_REPO}/${PAGILA_COMMIT}"

log() { printf '[fetch-pagila] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# Pick a SHA-256 tool: sha256sum (Linux/coreutils) or shasum (macOS/Perl).
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "need sha256sum or shasum on PATH"
fi

command -v curl >/dev/null 2>&1 || die "need curl on PATH"

mkdir -p "$DEST_DIR"

# fetch <file name> <expected sha256>
fetch() {
  local name="$1" expected="$2"
  local target="${DEST_DIR}/${name}"
  local tmp actual

  if [ -f "$target" ] && [ "$(sha256_of "$target")" = "$expected" ]; then
    log "${name}: already present, hash OK"
    return 0
  fi

  tmp="$(mktemp "${DEST_DIR}/.${name}.XXXXXX")"
  # shellcheck disable=SC2064 # expand $tmp now, on purpose
  trap "rm -f '$tmp'" EXIT
  log "${name}: downloading from ${BASE_URL}/${name}"
  curl --fail --silent --show-error --location --retry 3 \
    --output "$tmp" "${BASE_URL}/${name}"

  actual="$(sha256_of "$tmp")"
  if [ "$actual" != "$expected" ]; then
    rm -f "$tmp"
    die "${name}: sha256 mismatch (expected ${expected}, got ${actual})"
  fi
  chmod 0644 "$tmp"
  mv -f "$tmp" "$target"
  trap - EXIT
  log "${name}: verified and saved to ${target}"
}

fetch "pagila-schema.sql" "$PAGILA_SCHEMA_SHA256"
fetch "pagila-data.sql" "$PAGILA_DATA_SHA256"
log "done"
