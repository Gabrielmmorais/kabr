-- Kabr serve fixture. Runs once as the superuser in database "kabr_serve"
-- (testcontainers init script). TEST ONLY.
-- The API role is created without a password; the tests set a random one at
-- run time (internal/testutil.SetRolePasswords), so none is committed.
\set ON_ERROR_STOP on

CREATE TYPE public.mpaa_rating AS ENUM ('G', 'PG', 'PG-13', 'R', 'NC-17');
CREATE DOMAIN public.year AS integer CHECK (VALUE >= 1901 AND VALUE <= 2155);

-- film: integer key, more rows than the row limit, one column per encoding.
CREATE TABLE public.film (
    film_id          integer PRIMARY KEY,
    title            text NOT NULL,
    rating           public.mpaa_rating,
    release_year     public.year,
    rental_rate      numeric(4,2) NOT NULL,
    length_ratio     double precision,
    is_active        boolean NOT NULL DEFAULT true,
    last_update      timestamptz NOT NULL,
    released_on      date,
    poster           bytea,
    special_features text[],
    scores           integer[],
    fulltext         tsvector,
    meta             jsonb,
    uid              uuid
);
INSERT INTO public.film
SELECT g,
       'Film ' || g,
       (ARRAY['G','PG','PG-13','R','NC-17']::public.mpaa_rating[])[1 + g % 5],
       2000 + g % 20,
       (g % 5) + 0.99,
       g / 7.0,
       g % 3 <> 0,
       timestamptz '2026-01-01 00:00:00+00' + g * interval '1 hour 0.000123 second',
       date '2026-01-01' + g,
       decode(lpad(to_hex(g), 4, '0'), 'hex'),
       ARRAY['Trailers', 'Deleted Scenes'],
       ARRAY[g, g + 1],
       to_tsvector('simple', 'quick brown fox ' || g),
       jsonb_build_object('n', g, 'tags', jsonb_build_array('a', 'b')),
       ('00000000-0000-0000-0000-' || lpad(g::text, 12, '0'))::uuid
FROM generate_series(1, 250) AS g;
-- Row 1 has exact, known values; row 2 is all NULLs where allowed.
UPDATE public.film SET rental_rate = 4.99, length_ratio = 0.5, poster = '\x00ff10'::bytea,
    last_update = '2026-03-04 05:06:07.123456+00', released_on = '2006-05-25',
    meta = '{"a": [1, 2.50, "x"], "b": null}', release_year = 2006, rating = 'PG-13',
    fulltext = to_tsvector('simple', 'quick fox')
WHERE film_id = 1;
UPDATE public.film SET rating = NULL, release_year = NULL, length_ratio = NULL, released_on = NULL,
    poster = NULL, special_features = NULL, scores = NULL, fulltext = NULL, meta = NULL, uid = NULL
WHERE film_id = 2;
UPDATE public.film SET length_ratio = 'NaN' WHERE film_id = 3;

-- language: text key with characters that would matter if they ever
-- reached the SQL text.
CREATE TABLE public.language (
    code text PRIMARY KEY,
    name text NOT NULL
);
INSERT INTO public.language VALUES
    ('en', 'English'), ('pt-BR', 'Portuguese'), ('O''Brien', 'quote'),
    ('x'') OR 1=1 --', 'injection'), ('Ünïcødé', 'unicode'), ('with space', 'space'),
    ('$1', 'placeholder'), ('"quoted"', 'double quote'), ('A', 'upper'), ('a', 'lower');

-- rental_event: composite key with a timestamp (microseconds) in the middle.
CREATE TABLE public.rental_event (
    store_id    integer NOT NULL,
    happened_at timestamp without time zone NOT NULL,
    seq         smallint NOT NULL,
    note        text,
    PRIMARY KEY (store_id, happened_at, seq)
);
INSERT INTO public.rental_event
SELECT s, timestamp '2026-02-01 10:00:00' + t * interval '0.000001 second' * 1000003, q, 's' || s || 't' || t || 'q' || q
FROM generate_series(1, 3) AS s, generate_series(1, 25) AS t, generate_series(1, 2) AS q;

-- audit_log: timestamptz key, infinities and a BC timestamp included.
CREATE TABLE public.audit_log (
    at      timestamptz PRIMARY KEY,
    message text NOT NULL
);
INSERT INTO public.audit_log
SELECT timestamptz '2026-05-01 00:00:00+00' + g * interval '1.5 second', 'event ' || g
FROM generate_series(1, 30) AS g;
INSERT INTO public.audit_log VALUES
    ('infinity', 'end of time'), ('-infinity', 'start of time'),
    ('0044-03-15 12:00:00.000001+00 BC', 'ides of march');

-- customer: sensitive columns of every class and action.
CREATE TABLE public.customer (
    customer_id   integer PRIMARY KEY,
    first_name    text NOT NULL,
    email         text,
    password_hash text,
    card_number   text,
    active        boolean NOT NULL
);
INSERT INTO public.customer
SELECT g, 'Name' || g,
       CASE WHEN g % 4 = 0 THEN NULL ELSE 'user' || g || '@example.test' END,
       'h$' || md5(g::text), '4000-0000-0000-' || lpad(g::text, 4, '0'), g % 2 = 0
FROM generate_series(1, 20) AS g;

-- staging_import: no primary key, never served.
CREATE TABLE public.staging_import (
    line      text,
    loaded_at timestamptz
);
INSERT INTO public.staging_import VALUES ('a', now());

-- rating_info: enum key (paginated in text order).
CREATE TABLE public.rating_info (
    rating      public.mpaa_rating PRIMARY KEY,
    description text NOT NULL
);
INSERT INTO public.rating_info VALUES
    ('G', 'general'), ('PG', 'guidance'), ('PG-13', 'thirteen'), ('R', 'restricted'), ('NC-17', 'adults');

-- slow_table: every row is filtered by a policy that sleeps, so any read
-- of a row outlives a short statement_timeout. Reading zero rows is fast.
CREATE FUNCTION public.slow_true() RETURNS boolean
    LANGUAGE sql VOLATILE AS $$ SELECT pg_catalog.pg_sleep(3) IS NOT NULL $$;
CREATE TABLE public.slow_table (
    id integer PRIMARY KEY
);
INSERT INTO public.slow_table VALUES (1), (2);
ALTER TABLE public.slow_table ENABLE ROW LEVEL SECURITY;
CREATE POLICY slow_read ON public.slow_table FOR SELECT USING (public.slow_true());

-- drift: a schema of its own, changed by the drift test only.
CREATE SCHEMA drift;
CREATE TABLE drift.thing (
    id    integer PRIMARY KEY,
    label text NOT NULL
);
INSERT INTO drift.thing VALUES (1, 'one'), (2, 'two');

-- Roles ---------------------------------------------------------------------
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

-- kabr_api: what "kabr serve" connects as. LOGIN, CONNECT, USAGE and SELECT;
-- nothing else.
CREATE ROLE kabr_api LOGIN
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;
GRANT CONNECT ON DATABASE kabr_serve TO kabr_api;
GRANT USAGE ON SCHEMA public, drift TO kabr_api;
GRANT SELECT ON ALL TABLES IN SCHEMA public, drift TO kabr_api;

-- kabr_partial: may read public.film only (readiness probe test).
CREATE ROLE kabr_partial LOGIN
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;
GRANT CONNECT ON DATABASE kabr_serve TO kabr_partial;
GRANT USAGE ON SCHEMA public TO kabr_partial;
GRANT SELECT ON public.film TO kabr_partial;

ANALYZE;
