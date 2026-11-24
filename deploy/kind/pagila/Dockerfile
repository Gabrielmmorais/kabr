# syntax=docker/dockerfile:1
# kabr-pagila: PostgreSQL 17 with the Pagila sample database, for the local
# kind demo and the e2e test. LOCAL DEMO ONLY.
#
# The Pagila SQL (~3 MB) does not fit in a ConfigMap (1 MiB limit), so it is
# baked into this image together with the SAME initdb scripts the compose
# demo uses: the database comes out identical, and so does its catalog
# fingerprint (examples/pagila.schema.json, examples/pagila.plan.yaml).
#
# Build context: deploy/compose (see deploy/kind/pagila/Dockerfile.dockerignore;
# only initdb/ and the two fetched SQL files are sent). Run `make demo-fetch`
# first. No password is baked in: POSTGRES_PASSWORD, KABR_READER_PASSWORD and
# KABR_UNSAFE_PASSWORD come from a Secret at run time.
#
#   make pagila-image   # == docker build -f deploy/kind/pagila/Dockerfile -t kabr-pagila:dev deploy/compose

# Same image (tag and digest) as deploy/compose/docker-compose.yml.
FROM postgres:17.12-alpine@sha256:f9cf3e1f0066f0eb0c48707d864f0ac5941e9ff454a072077d97a4d278d07d73
COPY --chmod=0755 initdb/10-pagila.sh initdb/20-kabr-roles.sh /docker-entrypoint-initdb.d/
# /data must exist (0755) before the COPY: --chmod would otherwise also
# apply to the directory it creates, leaving it unreadable (no x bit).
RUN install -d -m 0755 /data
COPY --chmod=0644 data/pagila-schema.sql data/pagila-data.sql /data/
ENV POSTGRES_USER=postgres \
    POSTGRES_DB=pagila \
    PGDATA=/var/lib/postgresql/data/pgdata
# The alpine image's "postgres" user.
USER 70:70
EXPOSE 5432
