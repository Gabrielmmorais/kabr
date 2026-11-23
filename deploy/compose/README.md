# Demo database (PostgreSQL 17 + Pagila)

Local, throwaway PostgreSQL used to demo Kabr's read-only catalog
introspection. **Local demo only:** bound to `127.0.0.1`, never expose it.

```sh
make demo-up     # generate passwords, download + verify Pagila, start, wait for healthy
make demo-psql   # psql as kabr_reader
make demo-down   # stop, delete the volume and the generated passwords
```

| Item | Value |
|------|-------|
| Image | `postgres:17.12-alpine` |
| Host port | `127.0.0.1:55432` -> container `5432` |
| Database | `pagila` |
| Superuser | `postgres` / `$POSTGRES_PASSWORD` |
| Read-only role | `kabr_reader` / `$KABR_READER_PASSWORD` |
| Over-privileged role | `kabr_unsafe` / `$KABR_UNSAFE_PASSWORD` |

## Credentials

No password is committed, not even a demo one. `make demo-secrets` (run by
`make demo-up`) generates three random hex passwords into
`deploy/compose/.env.demo`, which is gitignored and created with mode `600`:

```sh
cat deploy/compose/.env.demo    # POSTGRES_PASSWORD, KABR_READER_PASSWORD, KABR_UNSAFE_PASSWORD
```

`docker-compose.yml` hands the file to the container with `env_file`;
`initdb/20-kabr-roles.sh` reads the role passwords with psql's `\getenv` and
quotes them with `:'var'`, so they never appear on a command line. The demo
make targets (`introspect-demo`, `introspect-demo-unsafe`, `test-demo`,
`demo-psql`) load the same file and pass the DSN to `kabr` through `KABR_DSN`.

The passwords are baked into the database volume on first start, so
`make demo-down` deletes the volume **and** `.env.demo` together; the next
`make demo-up` starts from scratch with new passwords. If you delete
`.env.demo` by hand while the volume still exists, run `make demo-down` before
`make demo-up` again.

- `kabr_reader`: `CONNECT` on the database, `USAGE` on `public`, `SELECT` on
  all tables (plus default privileges for future tables). This is how Kabr is
  meant to connect.
- `kabr_unsafe`: can `INSERT`/`UPDATE`/`DELETE`/`TRUNCATE` and `CREATE` in
  `public`. It exists only to trigger Kabr's least-privilege warning.
- The Pagila dump grants `ALL` on schema `public` to `PUBLIC` (pre-15
  default); `20-kabr-roles.sh` revokes `CREATE` from `PUBLIC` again so that
  `kabr_reader` really cannot create objects.

Init scripts in `initdb/` run once, in name order, when the volume is empty:
`10-pagila.sh` loads schema and data, `20-kabr-roles.sh` creates the roles.
To re-run them, `make demo-down` (drops the volume) and `make demo-up`.

## Pagila: source and license

[Pagila](https://github.com/devrimgunduz/pagila) by Devrim Gündüz, a PostgreSQL
port of MySQL's Sakila sample database, distributed under the
[PostgreSQL License](https://opensource.org/license/postgresql). The files are
not vendored in this repository: `scripts/fetch-pagila.sh` downloads
`pagila-schema.sql` and `pagila-data.sql` into `deploy/compose/data/`
(gitignored) from commit
[`fef9675`](https://github.com/devrimgunduz/pagila/tree/fef9675714cfba1756df4719b5e36075a7ddf90e)
(tag `pagila-v3.1.0`) and checks their SHA-256. (At that commit upstream's
`LICENSE.txt` still carried MIT-style text while the README named the
PostgreSQL License; upstream replaced it with the PostgreSQL License text in
4.1.2. Both are permissive.)

Version 3.1.0 is pinned on purpose: it is the last Pagila release that loads on
PostgreSQL 17. Pagila 4.x requires PostgreSQL 18 (`uuidv7()`, virtual
generated columns) and, from 4.1.0, the `pgvector` extension.
