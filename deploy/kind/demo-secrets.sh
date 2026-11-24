#!/usr/bin/env bash
# Create the random Secrets of the kind demo in namespace kabr-demo.
# LOCAL DEMO ONLY. Run by `make demo-db`; idempotent.
#
#   pagila-db        POSTGRES_PASSWORD, KABR_READER_PASSWORD, KABR_UNSAFE_PASSWORD
#                    (read by the Postgres pod's initdb scripts)
#   pagila-dsn       dsn = DSN of the read-only role kabr_reader (DataSource secretRef)
#   pagila-hash-key  key = HMAC key of "hash" columns (DataSource hashKeySecretRef)
#
# Values are random hex (valid in a URL without escaping). They are never
# printed, never passed on a command line (they go through files in a mode
# 700 temporary directory under bin/, deleted on exit) and never written to
# a tracked file. Existing Secrets are kept: the database is initialized
# with the passwords of pagila-db on first start, so regenerating them
# would lock the demo out; pagila-dsn is always rebuilt from pagila-db.
#
# Required environment (set by the Makefile; no ambient kubectl context is
# ever used):
#   KIND_KUBECONFIG  kubeconfig of the kind cluster (bin/kind-kabr.kubeconfig)
#   KUBE_CONTEXT     its context (kind-kabr)
set -euo pipefail

: "${KIND_KUBECONFIG:?set KIND_KUBECONFIG (kubeconfig of the kind cluster)}"
: "${KUBE_CONTEXT:?set KUBE_CONTEXT (e.g. kind-kabr)}"
NAMESPACE="${NAMESPACE:-kabr-demo}"
DB_HOST="${DB_HOST:-postgres.${NAMESPACE}.svc}"

kc() { kubectl --kubeconfig "$KIND_KUBECONFIG" --context "$KUBE_CONTEXT" -n "$NAMESPACE" "$@"; }
log() { printf '[demo-secrets] %s\n' "$*" >&2; }
rnd() { # rnd <bytes>: lowercase hex
  local v
  v="$(openssl rand -hex "$1" 2>/dev/null || od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n')"
  [ "${#v}" -eq $(($1 * 2)) ] || { log "could not generate a random value"; exit 1; }
  printf '%s' "$v"
}
# apply_secret <name> <kubectl create secret generic args...>
apply_secret() {
  local name="$1"; shift
  kc create secret generic "$name" "$@" --dry-run=client -o yaml | kc apply -f - >/dev/null
  kc label secret "$name" app.kubernetes.io/part-of=kabr-demo --overwrite >/dev/null
}

umask 077
mkdir -p bin
tmp="$(mktemp -d bin/.demo-secrets.XXXXXX)"
trap 'rm -rf "$tmp"' EXIT

if kc get secret pagila-db -o name >/dev/null 2>&1; then
  log "pagila-db exists, keeping its passwords"
  kc get secret pagila-db -o jsonpath='{.data.KABR_READER_PASSWORD}' | base64 -d > "$tmp/reader"
  [ -s "$tmp/reader" ] || { log "pagila-db has no KABR_READER_PASSWORD"; exit 1; }
else
  {
    printf 'POSTGRES_PASSWORD=%s\n' "$(rnd 16)"
    printf 'KABR_READER_PASSWORD=%s\n' "$(rnd 16)"
    printf 'KABR_UNSAFE_PASSWORD=%s\n' "$(rnd 16)"
  } > "$tmp/db.env"
  apply_secret pagila-db --from-env-file="$tmp/db.env"
  sed -n 's/^KABR_READER_PASSWORD=//p' "$tmp/db.env" | tr -d '\n' > "$tmp/reader"
  log "created pagila-db (random passwords)"
fi

printf 'postgres://kabr_reader:%s@%s:5432/pagila?sslmode=disable' "$(cat "$tmp/reader")" "$DB_HOST" > "$tmp/dsn"
apply_secret pagila-dsn --from-file=dsn="$tmp/dsn"
log "applied pagila-dsn (read-only role kabr_reader @ ${DB_HOST})"

if kc get secret pagila-hash-key -o name >/dev/null 2>&1; then
  log "pagila-hash-key exists, keeping it"
else
  rnd 32 > "$tmp/hashkey"
  apply_secret pagila-hash-key --from-file=key="$tmp/hashkey"
  log "created pagila-hash-key (random)"
fi
