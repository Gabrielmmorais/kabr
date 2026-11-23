-- Kabr introspection fixture. Runs once as the superuser in database
-- "kabr_fixture" (testcontainers init script). TEST ONLY.
-- The roles are created without a password; the tests set a random one at
-- run time (internal/testutil.SetRolePasswords), so none is committed.
\set ON_ERROR_STOP on

CREATE SCHEMA sales;
CREATE SCHEMA ignored;

-- Non-ASCII identifiers (same spirit as Pagila's "bıgınt" domain).
CREATE DOMAIN public."bıgınt" AS bigint;

-- customer: comments, defaults, unique constraint, expression index.
CREATE TABLE public.customer (
    id         serial PRIMARY KEY,
    email      text NOT NULL,
    name       varchar(80) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT customer_email_key UNIQUE (email)
);
COMMENT ON TABLE public.customer IS 'People who place orders';
COMMENT ON COLUMN public.customer.email IS 'Login e-mail, unique';
CREATE INDEX customer_lower_email_idx ON public.customer (lower(email));
CREATE INDEX customer_name_created_idx ON public.customer (name, created_at DESC);

-- employee: self-referencing, nullable FK, ON DELETE SET NULL.
CREATE TABLE public.employee (
    id         integer PRIMARY KEY,
    manager_id integer REFERENCES public.employee (id) ON DELETE SET NULL,
    full_name  text NOT NULL
);

-- sales.product: second schema.
CREATE TABLE sales.product (
    code  text PRIMARY KEY,
    title text NOT NULL,
    price numeric(10,2) NOT NULL DEFAULT 0
);
COMMENT ON TABLE sales.product IS 'Product catalog';

-- orders: composite primary key.
CREATE TABLE public.orders (
    region      char(2) NOT NULL,
    order_no    integer NOT NULL,
    customer_id integer NOT NULL REFERENCES public.customer (id) ON DELETE CASCADE,
    status      text NOT NULL DEFAULT 'new',
    PRIMARY KEY (region, order_no)
);

-- order_line: composite FK (columns in non-trivial order), cross-schema
-- nullable FK, SET DEFAULT action.
CREATE TABLE public.order_line (
    line_no      integer NOT NULL,
    order_no     integer NOT NULL,
    region       char(2) NOT NULL,
    product_code text DEFAULT 'UNKNOWN' REFERENCES sales.product (code) ON DELETE SET DEFAULT ON UPDATE CASCADE,
    qty          integer NOT NULL,
    PRIMARY KEY (region, order_no, line_no),
    CONSTRAINT order_line_order_fk FOREIGN KEY (region, order_no)
        REFERENCES public.orders (region, order_no) ON UPDATE CASCADE ON DELETE RESTRICT
);

-- orphan_log: no keys, no relationships.
CREATE TABLE public.orphan_log (
    logged_at timestamptz,
    message   text
);

-- measurement: partitioned table with 2 partitions.
CREATE TABLE public.measurement (
    city_id integer NOT NULL,
    logdate date NOT NULL,
    reading numeric(6,2),
    PRIMARY KEY (city_id, logdate)
) PARTITION BY RANGE (logdate);
COMMENT ON TABLE public.measurement IS 'Partitioned by month';
CREATE TABLE public.measurement_2026_01 PARTITION OF public.measurement
    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE public.measurement_2026_02 PARTITION OF public.measurement
    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');

-- measurement_note: FK referencing a partitioned table (PostgreSQL stores
-- extra internal per-partition constraints that must not be reported).
CREATE TABLE public.measurement_note (
    id      integer PRIMARY KEY,
    city_id integer NOT NULL,
    logdate date NOT NULL,
    note    text,
    FOREIGN KEY (city_id, logdate) REFERENCES public.measurement (city_id, logdate)
);

-- Non-ASCII table, column and domain-typed column.
CREATE TABLE public."café" (
    "código" public."bıgınt" PRIMARY KEY,
    "preço"  numeric(8,2)
);
COMMENT ON COLUMN public."café"."preço" IS 'Preço em reais';

-- Out of M1 scope: must not appear.
CREATE VIEW public.customer_v AS SELECT id, email FROM public.customer;
CREATE MATERIALIZED VIEW public.customer_mv AS SELECT id FROM public.customer;
CREATE TABLE ignored.hidden (id integer PRIMARY KEY);

-- Deterministic statistics: autovacuum is disabled on the server; only
-- these tables are analyzed, the others stay "never analyzed" (unknown).
INSERT INTO public.customer (email, name, created_at) VALUES
    ('a@example.com', 'Ann', '2026-01-01'), ('b@example.com', 'Bob', '2026-01-02'),
    ('c@example.com', 'Cid', '2026-01-03'), ('d@example.com', 'Bob', '2026-01-04');
INSERT INTO public.employee VALUES (1, NULL, 'Boss'), (2, 1, 'Dev'), (3, 1, 'Ops');
ANALYZE public.customer;
ANALYZE public.employee;

-- Roles ---------------------------------------------------------------------
-- PostgreSQL 15+ already grants no CREATE on public to PUBLIC; be explicit.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

-- kabr_reader: catalog-only. No SELECT on any table: introspection must
-- still work, proving Kabr never reads user rows.
CREATE ROLE kabr_reader LOGIN
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;
GRANT CONNECT ON DATABASE kabr_fixture TO kabr_reader;
GRANT USAGE ON SCHEMA public, sales TO kabr_reader;
REVOKE ALL ON ALL TABLES IN SCHEMA public, sales FROM kabr_reader;
REVOKE ALL ON ALL TABLES IN SCHEMA public, sales FROM PUBLIC;

-- kabr_unsafe: over-privileged on purpose.
CREATE ROLE kabr_unsafe LOGIN
  NOSUPERUSER CREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;
GRANT CONNECT ON DATABASE kabr_fixture TO kabr_unsafe;
GRANT USAGE, CREATE ON SCHEMA public TO kabr_unsafe;
GRANT USAGE ON SCHEMA sales TO kabr_unsafe;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public TO kabr_unsafe;
GRANT SELECT ON ALL TABLES IN SCHEMA sales TO kabr_unsafe;
