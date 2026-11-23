#!/bin/sh
# Load the Pagila schema and data into the demo database.
# Runs once, on first start of an empty volume, as the image superuser
# (POSTGRES_USER=postgres), which matches the "OWNER TO postgres" statements
# in the upstream dump. Files are fetched by scripts/fetch-pagila.sh and
# mounted read-only at /data.
set -eu

for f in /data/pagila-schema.sql /data/pagila-data.sql; do
  if [ ! -r "$f" ]; then
    echo "initdb: missing $f - run 'make demo-fetch' (scripts/fetch-pagila.sh) first" >&2
    exit 1
  fi
done

echo "initdb: loading Pagila schema"
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --quiet -f /data/pagila-schema.sql
echo "initdb: loading Pagila data"
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --quiet -f /data/pagila-data.sql
echo "initdb: Pagila loaded"
