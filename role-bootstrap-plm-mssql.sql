-- role-bootstrap-plm-mssql.sql — create/repair the read-only login for the
-- 1C PLM survey on sql02.office.playtoday.ru, database [plm].
--
-- Run under sysadmin (sa) on SQL02. DOCUMENTATION/ACTION script (the
-- login was expected to exist already: remote probes on 2026-09-26 showed
-- error 18456 for plm_reader while the server IS in mixed mode, so the
-- login is missing, disabled, or its password differs from the .env copy).
--
-- SECRETS HYGIENE: placeholders only — fill <strong-password-here> before
-- running and NEVER commit the filled copy. After running, put the SAME
-- password into .env (PLM_RO_USER_PWD) on the analyst VPS.

-- 0) Preflight: 0 = mixed mode (SQL logins accepted). Verified remotely
--    2026-09-26: already 0 — no restart needed.
SELECT SERVERPROPERTY('IsIntegratedSecurityOnly') AS is_windows_only;

-- 1) Server login: create if missing, otherwise enable + reset password.
IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'plm_reader')
BEGIN
    CREATE LOGIN plm_reader WITH PASSWORD = N'<strong-password-here>',
        CHECK_POLICY = ON, DEFAULT_DATABASE = [plm];
END
ELSE
BEGIN
    ALTER LOGIN plm_reader ENABLE;
    ALTER LOGIN plm_reader WITH PASSWORD = N'<strong-password-here>',
        DEFAULT_DATABASE = [plm];
END

-- 2) Database user + read-only role inside the PLM base.
USE [plm];
IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'plm_reader')
    CREATE USER plm_reader FOR LOGIN plm_reader;
ALTER ROLE db_datareader ADD MEMBER plm_reader;

-- 3) Verification (both must return 0 rows):
--    a) any non-SELECT privilege held by the user in [plm]:
SELECT pe.class_desc, pe.permission_name, pe.state_desc
FROM sys.database_permissions pe
JOIN sys.database_principals pr ON pr.principal_id = pe.grantee_principal_id
WHERE pr.name = N'plm_reader'
  AND pe.state IN ('G','W')
  AND pe.permission_name <> 'SELECT';
--    b) membership in any writer/admin role:
SELECT r.name AS role_name
FROM sys.database_role_members m
JOIN sys.database_roles r ON r.role_principal_id = m.role_principal_id
JOIN sys.database_principals m2 ON m2.principal_id = m.member_principal_id
WHERE m2.name = N'plm_reader'
  AND r.name IN (N'db_datawriter', N'db_owner', N'db_ddladmin', N'db_securityadmin');

-- 4) Smoke test from the analyst VPS (creds sourced from .env):
--      set -a; source .env; set +a
--      go run ./plm-survey -db plm -q "SELECT @@SERVERNAME AS srv, SUSER_SNAME() AS lg"
--    Expected: one row, srv=SQL02, lg=plm_reader.
