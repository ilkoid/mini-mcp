# mcp-pg-readonly

A tiny **read-only PostgreSQL MCP server** for the ZCode agent. Exposes three
non-destructive tools over stdio JSON-RPC:

| Tool | What it does |
|---|---|
| `pg_readonly_query` | Run a single SQL statement (`$1,$2` params) under the `mcp_ro` role, in a READ ONLY txn, with row/byte caps. |
| `pg_list_tables` | List tables in a schema (defaults to `public`). |
| `pg_describe_table` | Columns + PK/unique constraints + indexes for one table. |

Read-only is enforced **at the database** by a dedicated `mcp_ro` role (see
`role-bootstrap.sql`). The server adds belt-and-suspenders: a `READ ONLY`
transaction, a 30s `statement_timeout`, and 500-row / 64KB output caps. SQL
parsing is **not** the security boundary — it's only a DX precheck.

## Paths

| Artifact | Path |
|---|---|
| Sources | `~/dev/mini-mcp/` |
| Binary | `~/bin/mcp-pg-readonly` (stable path — used as the MCP `command`) |
| Wrapper | `~/bin/mcp-pg-readonly-run` (sources secrets → execs the binary) |
| Secrets | `~/.config/mcp-pg-readonly/env` (chmod 600) |
| Go module | `github.com/ilkoid/mcp-pg-readonly` |

> The folder is `mini-mcp`, but the module/binary name follows the design
> doc's canon (`mcp-pg-readonly`) so the user-scope MCP `command` path is
> stable across repo renames.

## Build & install

```bash
cd ~/dev/mini-mcp
go build -o ~/bin/mcp-pg-readonly .
chmod +x mcp-pg-readonly-run.sh
cp mcp-pg-readonly-run.sh ~/bin/mcp-pg-readonly-run

# secrets (fill PG_RO_PWD — and PG_ADMIN_PWD for the one-time bootstrap)
mkdir -p ~/.config/mcp-pg-readonly
chmod 700 ~/.config/mcp-pg-readonly
cat > ~/.config/mcp-pg-readonly/env <<'EOF'
export PG_RO_PWD='...'
export PG_ADMIN_PWD='...'   # only needed for role-bootstrap.sql, not at runtime
EOF
chmod 600 ~/.config/mcp-pg-readonly/env
```

`PG_RO_PWD` is **distinct** from `PG_PWD` (the admin/app password poncho-ai
downloaders use). Don't reuse them.

## One-time role bootstrap (Phase A)

```bash
set -a; source ~/.config/mcp-pg-readonly/env; set +a   # PG_RO_PWD, PG_ADMIN_PWD
PGPASSWORD="$PG_ADMIN_PWD" psql -h 192.168.10.7 -p 15432 -U postgres \
  -d wb_data_test -v pwd="$PG_RO_PWD" -f role-bootstrap.sql
# prod — only after smoke on test:
# PGPASSWORD="$PG_ADMIN_PWD" psql ... -d wb_data_prod -v pwd="$PG_RO_PWD" -f role-bootstrap.sql
```

**Before running**, find the real table owner so future tables are readable:
```sql
SELECT DISTINCT tableowner FROM pg_tables WHERE schemaname='public';
```
Then edit `role-bootstrap.sql` §4 — replace `postgres` in
`ALTER DEFAULT PRIVILEGES FOR ROLE postgres ...` with the real owner (repeat
the block if owners are mixed).

Phase B (`analytical` / `recommendation` schemas) lives in
`role-bootstrap-layers.sql` — run it only after those schemas exist.

## Selftest

```bash
~/bin/mcp-pg-readonly --selftest --database wb_data_test
```
Connects as `mcp_ro`, runs `SELECT 1`, then a write probe that **must fail**
(verifies the boundary end-to-end). Prints to stderr; exits non-zero on failure.
Never runs against prod implicitly — you choose the database via `--database`.

## Register in ZCode (user scope)

Create/edit **`~/.zcode/cli/config.json`** (NOT `~/.zcode/v2/config.json` —
that's the provider config). Add two server entries for prod/test isolation:

```jsonc
{
  "mcp": {
    "servers": {
      "pg-readonly": {
        "type": "stdio",
        "command": "/Users/ilkoid/bin/mcp-pg-readonly-run",
        "args": ["--database", "wb_data_prod"],
        "env": { "PGHOST": "192.168.10.7", "PGPORT": "15432" }
      },
      "pg-readonly-test": {
        "type": "stdio",
        "command": "/Users/ilkoid/bin/mcp-pg-readonly-run",
        "args": ["--database", "wb_data_test"],
        "env": { "PGHOST": "192.168.10.7", "PGPORT": "15432" }
      }
    }
  }
}
```

Rules (from `diagnosing-mcp`):
- `command` is an **absolute path string** (not an array — that triggers
  `command.trim is not a function`).
- Use `env`, not the legacy `environment`.
- Prod and test are **different server names** so a selftest can't hit prod.

Restart ZCode → **Settings → MCP** should show both servers green with
`toolCount ≥ 3`.

The password is **not** in this JSON — the wrapper sources it from
`~/.config/mcp-pg-readonly/env`.

## Multiple databases / VPS

This server runs **one MCP process per database** — there is no in-process
multi-DB mode, and a `connections.yaml` would just shadow what flags + env
already express. To connect a second VPS or database, register **another
server entry** in `~/.zcode/cli/config.json` with its own host/port/args. The
agent then sees each DB under its own tool namespace
(`mcp__pg-prod__…`, `mcp__pg-vps2__…`) and picks the right one per task.

The setup assumes a **shared `mcp_ro` role with the same password across all
hosts** (the default). That keeps one secrets file (`env`) working for every
server — each entry differs only by `PGHOST` / `PGPORT` / `PGSSLMODE` /
`--database`.

```jsonc
// ~/.zcode/cli/config.json
{
  "mcp": {
    "servers": {
      "pg-prod": {
        "type": "stdio",
        "command": "/Users/ilkoid/bin/mcp-pg-readonly-run",
        "args": ["--database", "wb_data_prod"],
        "env": { "PGHOST": "192.168.10.7", "PGPORT": "15432" }
      },
      "pg-vps2": {
        "type": "stdio",
        "command": "/Users/ilkoid/bin/mcp-pg-readonly-run",
        "args": ["--database", "main"],
        "env": {
          "PGHOST": "vps2.example.com", "PGPORT": "5432",
          "PGSSLMODE": "require"
        }
      }
    }
  }
}
```

### Per-host checklist

- **Bootstrap the `mcp_ro` role on every host** with `role-bootstrap.sql`.
  That role is the security boundary — its absence means no read-only
  guarantee, just SQL parsing.
- **Use `PGSSLMODE=require` (or `verify-full`) over the open internet.**
  `disable` is only fine on a LAN/trusted tunnel.
- **Distinct server names per DB** so a selftest can never hit the wrong host.
- **Keep the password out of `config.json`** — it stays in the shared
  `~/.config/mcp-pg-readonly/env`.

> Per-host password instead of a shared one? The wrapper sources a secrets file
> whose path can be overridden per server via `MCP_PG_SECRETS` (see
> `mcp-pg-readonly-run.sh`). Drop one file per host under
> `~/.config/mcp-pg-readonly/env.d/<name>` and point each server's `env` at it.

## Windows (VPS)

The repo ships two launchers with identical behavior — pick the one matching
your platform:

| Platform | Wrapper | Secrets file syntax |
|---|---|---|
| Linux / macOS | `mcp-pg-readonly-run.sh` | `export PG_RO_PWD='...'` |
| Windows | `mcp-pg-readonly-run.cmd` | `set PG_RO_PWD=...` (**no quotes!**) |

The Windows secrets file uses **`set`** (cmd syntax), **not** `export` and
**not** PowerShell `$env:`. It is sourced via `call`, so the vars reach the
`.exe`. Do **not** quote the value — cmd `set` treats the quotes as part of
the value, so `set PG_RO_PWD='abc'` stores literally `'abc'`.

> Build/run these in **cmd.exe**, not PowerShell — the wrapper was written for
> cmd. (ZCode itself invokes the `.cmd` via cmd.exe directly; no PowerShell or
> WSL layer is needed.)

### 1. Get the sources onto the VPS

Either `git clone`, or SCP the `~/dev/mini-mcp/` folder from your Mac. You
need the `.go` files, `go.mod`, `role-bootstrap.sql`, and
`mcp-pg-readonly-run.cmd`.

### 2. Build the `.exe` on the VPS (Go is installed there)

```cmd
cd %USERPROFILE%\dev\mini-mcp
mkdir %USERPROFILE%\bin 2>nul
go build -o %USERPROFILE%\bin\mcp-pg-readonly.exe .
copy mcp-pg-readonly-run.cmd %USERPROFILE%\bin\mcp-pg-readonly-run.cmd
```

> Don't copy the macOS-built binary (`Mach-O arm64`) to Windows — it won't
> run. Build the `.exe` on the VPS (or cross-compile with
> `GOOS=windows GOARCH=amd64 go build`).

### 3. Create the secrets file (run in PowerShell for ACL support)

```powershell
$dir = "$env:USERPROFILE\.config\mcp-pg-readonly"
New-Item -ItemType Directory -Force -Path $dir | Out-Null

# cmd `set` syntax, NOT export / $env:. Values must match what's in the DB.
# SAME password as on the Mac if you share one mcp_ro role across hosts.
$content = @'
set PG_RO_PWD=CHANGE_ME_mcp_ro_password
set PG_ADMIN_PWD=CHANGE_ME_postgres_admin_password
'@

$path = "$dir\env.cmd"
Set-Content -Path $path -Value $content -Encoding ASCII
# chmod 600 equivalent: strip inherited ACLs, leave only the current user.
icacls.exe $path /inheritance:r /grant:r "$($env:USERNAME):(R,W)"
Get-Content $path
```

> If ZCode runs as a **different user** than your RDP session, replace
> `$($env:USERNAME)` in the `icacls` line with that user — otherwise the
> wrapper can't read the file and you'll get "empty password".

### 4. Bootstrap the role — only if the VPS has its OWN database

**Skip this step if the VPS connects to the same PostgreSQL the Mac uses
(`192.168.10.7:15432`)** — `mcp_ro` is already created there. Only run
`role-bootstrap.sql` if the VPS has a separate PG instance:

```cmd
set PG_RO_PWD=CHANGE_ME_mcp_ro_password
set PG_ADMIN_PWD=CHANGE_ME_postgres_admin_password
"%ProgramFiles%\PostgreSQL\17\bin\psql.exe" -h 127.0.0.1 -p 5432 -U postgres ^
  -d wb_data_test -v ON_ERROR_STOP=1 -v pwd="%PG_RO_PWD%" ^
  -f %USERPROFILE%\dev\mini-mcp\role-bootstrap.sql
```

(adjust the psql path / host / port to your install)

### 5. Register in ZCode on the VPS

Edit `%USERPROFILE%\.zcode\cli\config.json`. Use **forward slashes** in the
path (ZCode's JSON parser is happier with them than bare backslashes), and
keep `command` as a **string** (not an array):

```jsonc
{
  "mcp": {
    "servers": {
      "pg-readonly": {
        "type": "stdio",
        "command": "C:/Users/YOUR_USER/bin/mcp-pg-readonly-run.cmd",
        "args": ["--database", "wb_data_prod"],
        "env": { "PGHOST": "192.168.10.7", "PGPORT": "15432" }
      }
    }
  }
}
```

Replace `YOUR_USER` with the actual Windows username. Add a
`pg-readonly-test` entry with `--database wb_data_test` if you want both.

### 6. Selftest on the VPS

```cmd
%USERPROFILE%\bin\mcp-pg-readonly-run.cmd --selftest --database wb_data_test
```

Expect `selftest: PASS`. Then **fully restart ZCode** on the VPS →
Settings → MCP → green, `toolCount: 3`.

### Windows gotchas

- **`set` value must not be quoted.** `set PG_RO_PWD=abc`, not `'abc'` —
  cmd stores the quotes literally.
- **`command` is a string, not an array**, and points at the **launcher**
  (`.cmd`), not the `.exe` directly — same as on Unix where it points at the
  `.sh` wrapper. The launcher sources the password.
- **`.cmd` is kept CRLF by convention** (see `.gitattributes`). cmd.exe
  tolerates LF for simple scripts, so if something misbehaves line endings
  are rarely the culprit — look at the secrets file and its `set` syntax
  first.
- **`PGSSLMODE`**: if the VPS reaches PG over the open internet, set
  `require` (or `verify-full`). `disable` is only fine on a LAN/tunnel.
- **No WSL/PowerShell layer.** The `.cmd` runs under plain `cmd.exe`. Don't
  wrap it in `powershell -File ...` — that adds an ExecutionPolicy layer for
  nothing.
- **`setx` is not recommended.** It writes the password to the registry
  (`HKCU\Environment`), visible to every process of your user. The `env.cmd`
  + ACL approach mirrors the Unix `chmod 600` design and keeps the secret in
  one place.

## Debug

```bash
# MCP inspector (interactive):
npx @modelcontextprotocol/inspector ~/bin/mcp-pg-readonly --database wb_data_test

# Raw JSON-RPC (watch that the binary writes NOTHING to stdout except frames):
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
  | ~/bin/mcp-pg-readonly-run --database wb_data_test
```

If Settings → MCP shows `toolCount=0` or the server fails to start: the binary
is almost certainly writing something other than protocol frames to stdout.
All logging here goes to stderr; don't add `fmt.Println`.

## Multi-session connection math

- Pool `MaxConns = 1` per stdio process (tool calls in a session are serial).
- Role `CONNECTION LIMIT = 8` globally — covers several ZCode sessions + the
  inspector + a selftest. Each session = one stdio process = one backend conn.
- If you see `too many connections for role "mcp_ro"`: raise the role limit,
  **don't** bump pool `MaxConns` (that would starve other sessions).

Monitor: `SELECT * FROM pg_stat_activity WHERE usename = 'mcp_ro';`

## JSON type notes

BIGINT (`int8`) and `numeric` are returned as **JSON strings** to avoid float64
precision loss (WB `nm_id` values exceed 2^53). Timestamps are RFC3339 UTC.
`json`/`jsonb` are embedded raw. Large `bytea` (>1KB) is rejected.

## UX caveat — approval in plan mode

The ZCode bundle currently sets `needsApproval: true` for **all** MCP tools
regardless of `readOnlyHint`. So in plan mode this may prompt for approval per
call (or per tool) — *more* friction than the patched silent `psql` in
`~/dev/zcode-tweaks`. **The bundle patch is intentionally kept in parallel.**

Removing the patch is a separate decision, only after SQL skills migrate to
MCP **or** "plan-mode SQL only via MCP" is adopted, and approval UX is
acceptable. See design doc §6.4 / §9.

Live-check after install: invoke `pg_readonly_query` from plan mode, note how
many approvals are prompted, and record the result here.

## Tests

```bash
go test ./...                                  # unit + annotations (no DB needed)
PG_TEST_DSN="postgres://mcp_ro:...@host:port/wb_data_test?sslmode=disable" \
  go test -run TestReadOnly -v ./...           # integration (skipped without DSN)
```

Never point `PG_TEST_DSN` at prod.

## Residual risks (honest)

- No row/column RLS in MVP — `mcp_ro` reads all granted business tables.
- A query can burn CPU/IO until `statement_timeout` (mitigated, not eliminated).
- `SECURITY DEFINER` functions with PUBLIC `EXECUTE` could still write — audit
  and narrow `EXECUTE` grants; this server is not a full sandbox.

## Design source

`/Users/ilkoid/dev/poncho-ai/reports/2026-07-25_mcp-pg-readonly-design.md` (Rev 2).
