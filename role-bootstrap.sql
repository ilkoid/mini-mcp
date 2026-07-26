-- role-bootstrap.sql — Phase A: create the mcp_ro read-only role.
--
-- Creates a project-independent role `mcp_ro` with SELECT-only privileges on
-- the `public` schema of the CURRENT database (whichever you connect to with
-- `psql -d <db>`). Run this once per database you want the MCP server to reach
-- (e.g. wb_data_test first, then wb_data_prod deliberately).
--
-- Security model: read-only is enforced at the DB layer (this role). The MCP
-- server adds a READ ONLY transaction + statement_timeout + caps on top, but
-- the role is the actual boundary — see design doc §4.1.
--
-- Prerequisites:
--   - You need PG admin/superuser to CREATE ROLE and GRANT.
--   - Know the REAL table owner for ALTER DEFAULT PRIVILEGES (see §5 below).
--
-- Usage:
--   set -a; source ~/.config/mcp-pg-readonly/env   # provides PG_RO_PWD
--   set +a
--   PGPASSWORD="$PG_ADMIN_PWD" psql -h 192.168.10.7 -p 15432 -U postgres \
--     -d wb_data_test -v pwd="$PG_RO_PWD" -f ~/dev/mini-mcp/role-bootstrap.sql
--   # prod — only after smoke on test, same Phase A:
--   # PGPASSWORD="$PG_ADMIN_PWD" psql ... -d wb_data_prod -v pwd="$PG_RO_PWD" -f ~/dev/mini-mcp/role-bootstrap.sql
--
-- NOTE on psql variables: substitution of :'pwd' does NOT happen inside
-- DO $$ ... $$ blocks (dollar-quoting is treated as a string literal). We use
-- \gexec instead: 'pwd' is interpolated in a regular SELECT and quoted safely
-- via format(%L). Stop on the first error by adding -v ON_ERROR_STOP=1.

-- 1. Idempotent role creation via \gexec. The CREATE ROLE is generated (and
--    password safely quoted with %L) only when the role does not yet exist.
--    CONNECTION LIMIT 8 covers a few concurrent ZCode sessions + the inspector
--    + a selftest (each stdio process = 1 backend conn at pool MaxConns=1).
SELECT format(
  'CREATE ROLE mcp_ro LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 8',
  :'pwd'
)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mcp_ro')
\gexec

-- 2. (Re)apply attributes + password idempotently. This is regular SQL, so
--    :'pwd' IS interpolated by psql here. Safe to re-run.
ALTER ROLE mcp_ro
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
  CONNECTION LIMIT 8;
ALTER ROLE mcp_ro PASSWORD :'pwd';

-- 3. Database-level CONNECT on the current database. GRANT needs a name, so
--    we resolve current_database() server-side and quote it with %I.
SELECT format('GRANT CONNECT ON DATABASE %I TO mcp_ro', current_database())
\gexec

-- 4. Schema public: USAGE + SELECT on all current tables.
GRANT USAGE ON SCHEMA public TO mcp_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO mcp_ro;
-- Sequences aren't needed for read-only analytics; uncomment if you hit a
-- nextval-using view:
-- GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO mcp_ro;

-- 5. Future tables: grant SELECT automatically.
--    IMPORTANT: ALTER DEFAULT PRIVILEGES records the grant for the role that
--    CREATES tables. poncho-ai downloaders create public tables as some owner
--    role (often NOT the superuser running this script). Replace `postgres`
--    below with the REAL owner, or future tables won't be readable by mcp_ro.
--    Find owners:  SELECT DISTINCT tableowner FROM pg_tables WHERE schemaname='public';
--    Repeat this block once per owner role if tables have mixed owners.
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public
  GRANT SELECT ON TABLES TO mcp_ro;
-- TODO(operator): if the real owner is, e.g., `poncho_dl`, also run:
--   ALTER DEFAULT PRIVILEGES FOR ROLE poncho_dl IN SCHEMA public
--     GRANT SELECT ON TABLES TO mcp_ro;

-- 6. Belt: explicitly strip writes (the role has no GRANT for them anyway,
--    but this defends against PUBLIC grants inherited from elsewhere).
REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER
  ON ALL TABLES IN SCHEMA public FROM mcp_ro;

-- 7. Residual risk (design §4.6): SECURITY DEFINER functions callable by
--    PUBLIC could still write. We do NOT blanket-REVOKE EXECUTE here (that
--    breaks basics). Audit writers and narrow EXECUTE grants individually.

-- Verification (optional, run after):
--   SELECT rolname, rolconnlimit FROM pg_roles WHERE rolname='mcp_ro';
--   \dp public.*
