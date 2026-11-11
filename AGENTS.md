# AGENTS.md — mcp-pg-readonly / mcp-mssql-readonly

Two tiny **read-only MCP servers** for the ZCode agent over stdio JSON-RPC,
sharing one **keyword-anonymization engine** (`internal/anon`, YAML rules,
fail-closed `--anon-config`):

- `mcp-pg-readonly` (PostgreSQL): tools `pg_readonly_query`,
  `pg_list_tables`, `pg_describe_table`.
- `mcp-mssql-readonly` (SQL Server / DWH): tools `mssql_readonly_query`,
  `mssql_list_dbs`, `mssql_list_tables`, `mssql_describe_table`.

Separate Go module (`mini-mcp`) — **not** part of poncho-ai. The folder is
`mini-mcp`; the binary names (`mcp-pg-readonly`, `mcp-mssql-readonly`) are
deliberate, so the user-scope MCP `command` paths stay stable across repo
renames. The anon engine and the result envelope (`internal/result`) are
DB-agnostic — keep them shared. Full PG design:
`/Users/ilkoid/dev/poncho-ai/reports/2026-07-25_mcp-pg-readonly-design.md`;
anon-guard design: `reports/2026-09-03_ТЗ_MCP_анонимизация.md`.

## Session output language

**Everything rendered to the user on screen is RUSSIAN — no exceptions.**
Chat replies, summaries, tables, plans, test reports, AskUserQuestion
options — any user-facing session output is written in Russian only (the
user explicitly requested this; they cannot read Chinese and want no other
language on screen). Code, identifiers, file comments, commit messages, and
log lines stay English as normal engineering hygiene; the boundary is
"what the user reads in the chat".

The repo also hosts the sibling module **`mcp-websearch/`** — a quota-free
local web search / page reader MCP (see the last table row).

## Build, test, run

```bash
go build -o ~/bin/mcp-pg-readonly ./cmd/mcp-pg-readonly        # PG binary
go build -o ~/bin/mcp-mssql-readonly ./cmd/mcp-mssql-readonly  # MSSQL binary
go test ./...                                                 # unit (no DB needed)
PG_TEST_DSN="postgres://mcp_ro:...@host:port/db?sslmode=disable" \
  go test -run TestReadOnly -v ./...                          # PG integration (skipped w/o DSN)
MSSQL_TEST_DSN="sqlserver://ai_reader:...@host:1433?database=db" \
  go test -run TestMSSQL -v ./internal/mssql                  # MSSQL integration (skipped w/o DSN)
~/bin/mcp-pg-readonly --selftest --database wb_data_test       # end-to-end RO + anon check
~/bin/mcp-mssql-readonly --selftest --database DWH_MART        # same for MSSQL
```

- `--database` is **required** in both binaries (no silent prod default).
- `--anon-config` is **required** in both (PG default:
  `~/.config/mcp-pg-readonly/anonymize.yaml`; MSSQL default:
  `~/.config/mcp-mssql-readonly/anonymize.yaml`; the VPS deployment points
  BOTH at the shared `E:\ARM_AI\.config\mcp-pg-readonly\anonymize.yaml`).
  `rules: []` = deliberate no-masking instance. The rules file is a SECRET —
  see Secrets hygiene.
- **Never** point `PG_TEST_DSN`/`MSSQL_TEST_DSN` at prod working DBs.
- No separate lint/typecheck script; `go build`/`go vet` are the gate.

## Files

| File | Role |
|---|---|
| `cmd/mcp-pg-readonly/main.go` | PG entry, flags, DSN, `runSelftest`. |
| `cmd/mcp-mssql-readonly/main.go` | MSSQL entry: mirror flags (+`--encrypt`), DWH endpoint defaults (`MSSQL_HOST`/`MSSQL_PORT`), `runSelftest` with catalog grant checks (SQL Server has no RO txn — the grants ARE the boundary verification). |
| `internal/anon/anon.go` | **Anonymization engine**: YAML rules → `Apply` (mask out) / `Unmap` (alias→real, before execution) / `ApplyJSON` / numeric rules / eq_any two-era expansion / GuardFragments / CheckBlockedFunctions. DB-agnostic — shared by both binaries. |
| `internal/anon/textual.go` | `DecodeTextual` (UTF-8/UTF-16-BOM decode) backing the binary guards of both dialects. |
| `internal/result/result.go` | **Shared result envelope**: `Result` JSON shape, caps (4000 rows / 48KB), `Builder`, truncation notice, empty-result heuristic. |
| `internal/pg/tools.go` | PG MCP tool registration, shared annotations, `DescribeTable`, masked error results (`toolErr`). |
| `internal/pg/ro.go` | PG **security core**: `RunReadOnly` (RO txn + statement_timeout), `RejectUnsafeSQL`, `NewPool`, PG blocked-function list, `renderPGComparison`. |
| `internal/pg/encode.go` | PG→JSON type mapping (int8/numeric as strings, etc.) + cell masking (strings, json/jsonb walk, numeric keywords, bytea guard). |
| `internal/mssql/conn.go` | sqlserver:// DSN builder via `mssql.NewConnector` (`SessionInitSQL = SET LANGUAGE us_english` — the DWH login's default language is Swedish, and error texts arrived in Swedish until 2026-09-04), single-connection pool (`MaxOpenConns=1`). |
| `internal/mssql/precheck.go` | T-SQL `RejectUnsafeSQL`: single statement (literals `N'…'`, `[brackets]`, comments, `GO` batches), leading-keyword deny-list (incl. EXEC/sp_*/DECLARE/USE), MSSQL blocked-function list, CAST/CONVERT-to-VARBINARY pattern. |
| `internal/mssql/ro.go` | MSSQL execution core: Unmap → precheck → eq_any expansion (`renderTSQLComparison` → `IN (N'…')`) → ctx timeouts → guards on output. |
| `internal/mssql/encode.go` | T-SQL→JSON mapping by `DatabaseTypeName()`: BIGINT/DECIMAL/MONEY → strings, UNIQUEIDENTIFIER → canonical text (TDS byte order!), TIME → clock text, VARBINARY → binary guard, NVARCHAR/XML → Apply+Guard. |
| `internal/mssql/tools.go` | MSSQL MCP tool registration (4 tools), `DescribeTable` via sys.columns/sys.key_constraints + sys.indexes (key columns; added 2026-09-04 after the live test report), role/env-interpolated description. |
| `anonymize.example.yaml` | Template for the SECRET rules file (`anonymize.yaml` is gitignored). |
| `role-bootstrap.sql` | PG Phase A — creates the `mcp_ro` role (the real boundary). |
| `role-bootstrap-layers.sql` | PG Phase B placeholder (`analytical`/`recommendation` schemas don't exist yet — intentionally inert). |
| `role-bootstrap-mssql.sql` | MSSQL role posture: DOCUMENTATION/VERIFICATION only (`ai_reader` already exists on the DWH; records the intended grants + read-only verification queries). |
| `mcp-pg-readonly-run.sh` | Secret wrapper installed at `~/bin/mcp-pg-readonly-run`; sources `~/.config/mcp-pg-readonly/env` (path overridable via `MCP_PG_SECRETS`), then execs the binary. Unix launcher. |
| `mcp-pg-readonly-run.cmd` | Windows equivalent of `.sh`. Sources `%USERPROFILE%\.config\mcp-pg-readonly\env.cmd` (note: `.cmd` ext, `set` syntax — NOT `export`), then execs `mcp-pg-readonly.exe`. Same `MCP_PG_SECRETS` override. |
| `mcp-pg-readonly-run-vps.cmd` | Shared-VPS variant (multi-analyst, DEPLOY-WINDOWS.md §7.1): exe in `%PUBLIC%\bin` (override `MCP_PG_BIN`), secrets default to `E:\ARM_AI\.config\mcp-pg-readonly\env.cmd` and PGHOST/PGPORT default to the deployment endpoint (`10.120.24.155:5432`) so bare-console runs work. Installed as `C:\Users\Public\bin\mcp-pg-readonly-run.cmd`. |
| `mcp-mssql-readonly-run.cmd` | MSSQL shared-VPS wrapper: exe `%PUBLIC%\bin\mcp-mssql-readonly.exe` (override `MCP_MSSQL_BIN`), secrets default `E:\ARM_AI\.config\mcp-mssql-readonly\env.cmd` (`set MSSQL_AI_PWD=…`; override `MCP_MSSQL_SECRETS`), MSSQL_HOST/MSSQL_PORT default to the DWH endpoint (`10.120.11.233:1433`). Installed as `C:\Users\Public\bin\mcp-mssql-readonly-run.cmd`. |
| `zcode-workspace-config.example.json` | Template for the ANALYSTS' project `.zcode/config.json` (workspace scope: committed to their repo, auto-connects for everyone). Real host/role, no secrets. |
| `.gitattributes` | CRLF hygiene: `*.cmd`/`*.bat` forced to CRLF (cmd breaks on LF), everything else LF, `*.exe` binary. |
| `*_test.go` | Unit (`ro_test.go`, `tools_test.go`, `guards_test.go`, `guards_pg_test.go`, mssql `precheck_test.go`/`encode_test.go`) + integration (`ro_integration_test.go` ← `PG_TEST_DSN`; mssql `ro_integration_test.go` ← `MSSQL_TEST_DSN`). |
| `DEPLOY-WINDOWS.md` | Full from-scratch Windows VPS deploy guide (Russian): prerequisites, network to the PG host, secrets/ACL, role bootstrap, ZCode registration, git/secrets rules, troubleshooting, checklist. Keep in sync with the README "Windows (VPS)" quick section when behavior changes. |
| `mcp-websearch/` | Second module (`github.com/ilkoid/mcp-websearch`, own go.mod): quota-free local web search / page reader + level-4 real-browser bridge. Tools: `web_search` (plain-HTTP DDG/Bing scrape, auto-fallback), `read_url` (readable-text extraction, 48KB cap, charset-aware), `bridge_status` / `profile_fetch` / `profile_tab` (level 4 — see the bridge section below). Registered in BOTH ZCode (`~/.zcode/cli/config.json`) and AutoClaw (`~/.openclaw-autoclaw/openclaw.json`) so neither agent burns the hosted web-search/web-reader quota; steering rules live in `~/.zcode/AGENTS.md` and the auto-coder workspace AGENTS.md. `--selftest` probes the live engines; `--selftest-bridge` (needs the extension loaded) and one-shot `--profile-fetch URL` / `--profile-tab URL` exercise level 4. |
| `mcp-websearch/bridge.go` | Level-4 core: mailbox hub (ONE job slot + ONE result slot on one mutex — busy = immediate refusal, never a queue), lazy owner election on `127.0.0.1:8765` (first process binds = owner; siblings become /api clients; owner death → connection refused → re-election → takeover ~5s), `X-Bridge-Token` auth (shared file `~/.config/mcp-browserbridge/token`, 0600, auto-created; 401 without it — also closes DNS-rebinding via a loopback-Host check), server-enforced pacing `--min-interval` (2s default, gateIssue debt is paid by real sleep so bursts can't pile up a wait). Sequential by design: the ONLY goroutine the module ever starts is the owner's HTTP listener. |
| `mcp-websearch/extension/` | The MV3 browser extension (vanilla JS, no build step): `background.js` short-polls `GET /job` ~1/s (the poll doubles as the MV3 keepalive; `chrome.alarms` 30s is the fallback), executes `fetch` (profile cookies + fingerprint, charset sniff, 5MB cap) and `tab_read` (one reused pinned visible tab → title/url/innerText via `chrome.scripting`); `options.html/js` holds the token. Install/load steps + port caveat live in `extension/README.md`. |
| `WEB-PARSING-GUIDE.md` | Handoff guide (Russian): the full tool ladder (websearch MCP → read_url → IAB browser → own bridge level 4), mcp-websearch setup/registration for ZCode+AutoClaw, empirical applicability boundaries (incl. the 2026-11-12 result: **WB passes level 4 from a real profile** — bare fetch, one polite attempt max), and the battle-tested Ozon parsing methodology (one reused tab, 3s pacing, SKU-from-URL, webPrice/material selectors, anti-WAF etiquette). Written for the sibling parsing project; keep in sync with real selector drift. |

## Architecture rules that matter for edits

1. **The DB role is the security boundary — not SQL parsing.** `mcp_ro` (created
   by `role-bootstrap.sql`) has SELECT-only privileges. `rejectUnsafeSQL` in
   `ro.go` is a **DX precheck only**; a malicious payload that slipped past it
   is still stopped by the READ ONLY txn + role ACLs. Don't refactor the
   precheck into "the" boundary, and don't weaken `runReadOnly`'s `pgx.ReadOnly`
   txn / `statement_timeout` / rollback-always posture assuming the precheck
   holds.

2. **Stdout is sacred.** Stdio transport carries **only** MCP protocol frames
   on stdout. Any stray byte (a `fmt.Println`, a debug log) makes ZCode show
   `toolCount=0`. All logging goes to **stderr** (`log.SetOutput(os.Stderr)`
   in `main.go`). Never add stdout writes outside the SDK.

3. **`destructiveHint` must stay `*bool` with `ptr(false)`.** In `tools.go`,
   `readOnlyAnnotations` uses pointer fields for `DestructiveHint` and
   `OpenWorldHint` because the SDK tags them `omitempty`; a plain `false` would
   be dropped from the wire JSON. The ZCode plan-mode predicate is
   `isMcp && !destructive`, so omission breaks plan-mode auto-approval.
   `TestAnnotations_WireSerialization` / `TestToolAnnotations_SDKTypeShape` in
   `tools_test.go` are **load-bearing** — if they fail after an SDK bump,
   re-audit before shipping.

4. **Single-statement SQL only (both dialects).** `rejectUnsafeSQL` rejects
   empty, multi statement (any real `;` other than one optional trailing —
   semicolons inside string literals / dollar-quotes / comments / T-SQL
   `[bracket identifiers]` are data, not separators; checks run on a
   `maskLiteralsAndComments` copy; the MSSQL variant additionally rejects
   standalone `GO` batch lines), and leading non-read keywords (PG incl.
   session statements; MSSQL incl. EXEC / `sp_*` / DECLARE / USE). Agents
   must pass values via `$1,$2` (PG) or `@p1,@p2` (MSSQL) params, not
   inline. Keep this rule when editing. **Masked-copy scanner lesson
   (live leak 2026-09-04, mssql):** a T-SQL `[bracket identifier]` OPENS on
   `[` and CLOSES on `]` — the scanner hunted for the next `[` instead, so
   everything between two brackets (incl. `HASHBYTES(` / `LEFT(` names) was
   blanked as "literal" and the blocked-function guard went blind on
   bracketed Cyrillic columns while ASCII probes stayed green. Regression:
   `TestMaskLiteralsAndComments_BracketShape` + the Cyrillic rows in
   `TestRejectUnsafeSQL_BlockedLeakFunctions` (precheck_test.go) are
   load-bearing; keep the masked shape (function names VISIBLE, bracket
   content blanked) asserted when touching the scanner.

5. **Fixed result envelope.** `Result{columns, row_count, truncated, notice,
   rows}` is the contract with the agent — metadata BEFORE rows by design:
   the ZCode client chops the JSON tail of oversized output (~50KB
   resultBudget), and this order keeps `truncated`/`notice` readable when
   that happens. Caps: `maxRows=4000` (500 → 2000 → 4000, owner decisions 2026-09-04 — analytical chunking pain; the 48KB byte cap is the real bound set by the client budget), `maxBytes=48KB` (must stay below the
   client budget incl. envelope overhead). Double truncation (server cap +
   client budget) is intentional; keep server caps ≤ client budget. A single
   row exceeding `maxBytes` on its own gets a dedicated notice (advise a
   narrower projection — NOT `length()/left()/substring()`: those functions
   are blocked by the anonymization guard, see rule 10), distinct from the
   too-many-rows notice.

6. **Pool discipline.** `MaxConns = 1` per stdio process (tool calls are
   serial). Role `CONNECTION LIMIT = 8` covers several sessions + inspector +
   selftest. If you see `too many connections for role "mcp_ro"`, **raise the
   role limit** — do **not** bump pool `MaxConns` (starves other sessions).
   `statement_timeout` is set per-query via `set_config(..., true)` (NOT
   globally in `newPool`, and NOT via `SET LOCAL $1` which PG doesn't support).

7. **Three timeout layers; keep the order.** PG `statement_timeout` (default
   **2min**, flag `--statement-timeout`) < whole-call ctx deadline (default
   **3min**, flag `--query-timeout`) < ZCode client `timeoutMs` (per-entry
   values — see the registration section, currently 800000 on pg-anon and
   400000 elsewhere; the client default is 30s!). Both
   binaries refuse to start if query-timeout ≤ statement-timeout, so the
   DB-side cancel (57014, wrapped with a hint by `wrapQueryError`) always
   fires first. The `set_config` value is sent as **plain milliseconds** —
   PG does not parse Go's `2m0s` duration format (selftest catches this).
   MSSQL note: the driver exposes no server-side statement_timeout, so BOTH
   layers are client-side contexts there (statement ctx cancels the in-flight
   TDS query first, call ctx wraps scan/cleanup). Don't remove timeouts:
   they cap damage to the shared instance, and while one query runs, that
   session's next tool call waits (`MaxConns=1`).

8. **Anonymization is a bidirectional choke point, not a feature flag.**
   Every string leaving `internal/pg` goes through `Anonymizer.Apply`
   (cells incl. json/jsonb interiors, column names, notices, ERROR TEXTS —
   DB errors echo values); every SQL text / string bind from the agent goes
   through `Unmap` BEFORE execution, so the agent's alias filters hit real
   rows and the session never sees an original in either direction. When
   adding a new tool or output path, wire BOTH directions — a path that
   skips masking is a data leak, and a path that skips Unmap silently
   breaks alias filtering. The rules file is a SECRET: report counts only,
   never values; validation errors use rule indexes, not contents.

9. **No writes to working databases — not even "expected to fail" probes.**
   Selftest's write probe is CLIENT-side (`RejectUnsafeSQL` rejects before
   anything is sent); the read-only posture is verified via the grants
   catalog (`information_schema.role_table_grants`) and
   `SHOW transaction_read_only`. Never reintroduce a server-side write
   probe (the old `CREATE TEMP TABLE` probe sent a write statement to the
   DB); the role + RO txn remain the actual boundary.

10. **Derivative guards — masking full keywords is not enough** (independent
    test report `reports/2026-09-03_ТЗ_MCP_анонимизация.md`, Д-1/Д-2/Д-3).
    Functions computed in the DB (`left(col,4)`, `md5(col)`, `length(col)`)
    ship derivatives that substring masking never touches. Three layers, all
    in the shared `internal/anon` engine + thin dialect wiring:
    (a) `CheckBlockedFunctions` in `RejectUnsafeSQL` — query-wide blocklist
    of substring/hash/length/oracle families on the literal-masked copy
    (PG list = `defaultBlockedFunctions` in `ro.go`; the SECRET yaml can
    replace it via `security.blocked_functions_override`); (b)
    `GuardFragments` — output-side redaction ("…") of any ≥4-char
    (configurable) keyword run that survived Apply, alias-shielded, wired
    into string cells, jsonb interiors, notices, error texts
    (`guardedApply`), and bytea text (utf-8/utf-16-BOM decode first);
    (c) NOT applied to column names (false positives like `played_at` would
    break introspection — the cell guard covers the leak vector). Residual,
    documented: boolean oracles (`CASE WHEN col LIKE '_l__'`) need a
    column-mapping white-list parser (deferred "этап C").
11. **Two-era data & filter completeness** (same report, Д-4/Д-5/Д-6/Д-7).
    An upstream autonomous anonymizer wrote the ALIAS into newer rows, so
    the DB holds keyword-era AND literal-alias-era values — the shared alias
    is a DELIBERATE owner decision (never "fix" it by renaming aliases).
    Consequences implemented: `eq_any` per rule (every stored spelling,
    including the literal alias) expands full-literal `=`/`LIKE`/`ILIKE`
    filters AFTER `Unmap` (PG render: `= ANY(ARRAY[...])`; T-SQL render:
    `IN (N'...')` — there is no LIKE ANY in T-SQL and a full-literal LIKE is
    equality-equivalent under CI/CS collations; the literal scanner also
    understands the T-SQL `N'…'` national-string prefix, which the rewrite
    absorbs into the replaced span; wildcard patterns and E'' literals never
    rewritten; bind args covered only by the 0-rows notice); `upperIfUpper`
    renders all-upper tokens with the upper alias (unifies eras under
    `upper()`); zero-rows + `=`-by-alias and partial-pattern hints land in
    `notice` (detectors judge the ORIGINAL pre-Unmap SQL/args snapshot).
    GROUP BY may legitimately show two rows with one visible alias — do not
    "deduplicate" that.

## JSON type encoding (`encode.go`)

- **int8 / numeric → JSON string** (WB `nm_id` exceeds 2^53; float64 would
  corrupt it). Guarded by `TestEncodeInt8AsString`. Agents cast `::int` /
  `::float8` in SQL when they need a JSON number — this is hinted in the
  `pg_readonly_query` description (numeric arithmetic like `round(100.0*x/y)`
  trips on it otherwise).
- Timestamps → RFC3339 UTC. `json`/`jsonb` embedded raw. `bytea` > 1KB rejected.

## Conventions

- Go 1.25, `package main`, single binary. Style: dense comments referencing
  design-doc sections (§4.x, §5.0) — preserve those references when editing.
- Tool errors are surfaced inside `CallToolResult` with `IsError=true` (MCP
  spec), **not** as protocol errors, so the LLM can self-correct.
- `PG_RO_PWD` (this server) is **distinct** from `PG_PWD` (admin/app
  downloaders in poncho-ai). Don't reuse them.

## ZCode registration (for reference)

Servers are registered in **`~/.zcode/cli/config.json`** (NOT
`~/.zcode/v2/config.json`, which is provider config). `command` is an absolute
path string (not an array); args/env go in the sibling `args`/`env` fields.
Prod and test are different server names (`pg-anon` / `pg-anon-test`) so a
selftest can't hit prod. Password stays out of that JSON — the wrapper
sources it. Per-entry timeouts (owner decision 2026-09-04, doubled after live
2min statement timeouts on multi-subquery analytical queries): the prod entry
`pg-anon` runs `--statement-timeout 10m --query-timeout 12m` +
`timeoutMs: 800000`; every other entry (`pg-anon-test`, all four MSSQL) runs
`--statement-timeout 4m --query-timeout 6m` + `timeoutMs: 400000` (layering
statement < call < client kept). The
MSSQL entries `mssql-readonly` (DWH_MART) and `mssql-readonly-bi`
(bi_playtoday — the WB/Ozon/LAMODA/mpstats BI base; role `ai_reader` /
`MSSQL_AI_PWD`, `10.120.11.233:1433` for both) point `--anon-config` at the
SAME shared `anonymize.yaml` as the PG entries — one rules file masks every
server. Note: the bi_playtoday NAME contains a rule keyword, so it displays
masked in tool output; three-part queries by the masked name are Unmapped
back transparently.

The sql02 1C entries (added 2026-11, owner decision 2026-11-10): wrappers
`C:\Users\Public\bin\mcp-mssql-plm-readonly-run.cmd` (db `plm`, role
`plm_reader` / `PLM_RO_USER_PWD`) and `...\mcp-mssql-wms-readonly-run.cmd`
(db `WMs`, `wms_reader` / `WMS_RO_USER_PWD`), host `10.120.11.24:1433`,
secrets in `E:\ARM_AI\.config\mcp-mssql-readonly\{plm,wms}.env.cmd`. They
run the same 4m/6m + `timeoutMs: 400000` and ALSO point at the shared
`anonymize.yaml` (3 rules) — per-base `plm-/wms-anonymize.yaml` are kept
on disk UNUSED, placeholders for future 1C-specific keywords. Survey
context: `reports/2026-09-26_plm-1c-survey.md`.

An entry may run as a different **existing** read-only role instead of
`mcp_ro` via `--user` / `--password-env` (e.g. poncho-ai prod:
`arm_ai_ro` / `PG_USER_PWD`, `10.120.24.155:5432`, db `wb_data_prod`).
`tools.go` interpolates the role + env-var name into the `pg_readonly_query`
description (`queryDescriptionFmt`) — keep them parameterized when editing
that text; a hardcoded `mcp_ro` misdirects the agent's psql bypass on other
roles. `buildQueryDescription` also names the `--database` and the configured
`PGHOST:PGPORT` (not `inet_server_addr()`), and leads with `⚠ PRODUCTION
database` when the database matches `*_prod`: `pg-readonly` (prod) and
`pg-readonly-test` run the same binary on the same host:port and differ only
by database name, so that text is the agent's only prod/test signal. The psql bypass is only as safe as the role's grants, so an
existing-role entry presumes the role was verified SELECT-only
(`role_table_grants` check, see README / DEPLOY-WINDOWS.md variant Б).

## mcp-websearch: level-4 bridge (real browser)

The bridge passes WB-class anti-bot walls by running requests inside the
user's REAL browser profile (Edge on macOS / clean Chrome on the Windows
VPS) via the extension. Verified 2026-11-12: WB main page returns HTTP 200
to a **bare fetch** from the profile; Ozon main too (its `__rr` challenge
resolves on profile cookies); Ozon SERP needs `profile_tab` (JS render);
Google/ya.ru searches pass with no captcha; playtoday.ru works both ways.

Rules for editing bridge code:

1. **Sequential by design.** One mailbox, one job slot, one result slot; the
   only goroutine is the owner's `http.Server` listener (`go srv.Serve`).
   Waiting = 250ms short-poll on `takeResult(id)`; contention = immediate
   `busyError` (never a queue, never a second goroutine).
2. **Results match by job id** (`mailbox.takeResult`); a late answer to an
   old job must never satisfy a new waiter, and a timed-out job frees the
   slot (`dropJob`) so the bridge can't wedge busy.
3. **Pacing is server-side and global** (`gateIssue`, `--min-interval` 2s):
   the owner gates ALL jobs (own + every client's /api call), so several
   agent processes cannot outrun human pace. The debt is paid with real
   sleep — it never accumulates past one interval.
4. **Token + loopback only.** Every endpoint requires `X-Bridge-Token`
   (shared file, 0600) and a loopback Host header. The token is NEVER
   committed; the extension gets it pasted via its options page.
5. **Etiquette is policy, not config.** One polite attempt per hostile site
   per session (WB: fetch → at most one tab → stop); never wait out
   anti-bot timers; scale data comes from the official pipeline
   (poncho-ai downloaders), not from this bridge. Plan-Б if the extension
   route ever dies: chromedp on a dedicated profile (≈ IAB trust level —
   i.e. probably NOT enough for WB; that's why it's plan-Б).

## Cross-platform (Unix ↔ Windows)

- **Two launchers, one binary.** `mcp-pg-readonly-run.sh` (Unix) and
  `mcp-pg-readonly-run.cmd` (Windows) are behaviorally identical: read a
  secrets file, then exec the binary with forwarded args. `command` in
  `config.json` points at whichever launcher matches the host.
- **Secrets syntax differs.** Unix: `export PG_RO_PWD='...'` in `env`.
  Windows: `set PG_RO_PWD=...` in `env.cmd` (**no quotes** — cmd stores them
  literally, so `set X='abc'` → value is `'abc'`). Don't mix — cmd can't
  source `export`, sh can't source `set`. Same shared `mcp_ro` password works
  across both.
- **Build per-OS.** `go build -o ~/bin/mcp-pg-readonly .` on Unix;
  `go build -o %USERPROFILE%\bin\mcp-pg-readonly.exe .` on Windows. The binary
  is OS-arch-specific — don't copy an `.exe` to a Linux VPS or vice versa.
- **`.gitattributes` is convention, not load-bearing.** `*.cmd`/`*.bat` are
  kept CRLF (canonical batch format); everything else LF, `*.exe` binary.
  Order matters — catch-all `*` goes first (last-match-wins). cmd.exe
  tolerates LF for simple `.cmd`, so line endings are rarely the root cause;
  don't send a debugger there first.
- **No WSL/PowerShell layer.** The `.cmd` runs under plain `cmd.exe`. Don't
  wrap it in `powershell -File ...` — adds ExecutionPolicy friction for nothing.

## Secrets hygiene — NEVER commit real secrets

This rule exists because of a real incident: a one-off "deployment
copy-paste" doc (commit `90be85f`, since purged) shipped both `PG_RO_PWD`
and `PG_ADMIN_PWD` to the **public** repo. The fix (history rewrite +
force-push) did **not** remove the leaked commit from GitHub — orphan
commits stay reachable by direct commit URL for ~90 days after a
force-push. `git log -p` shows nothing (the orphan is off-branch), but
the password is still downloadable in a browser. The only real
mitigation was rotating the password at the server.

**Hard rules for any doc, example, or comment in this repo:**

1. **Placeholders only, never real values.** Write `set PG_RO_PWD=<your_mcp_ro_password>`,
   never `set PG_RO_PWD=actual_password`. This applies to README, AGENTS.md,
   SQL, `.cmd`/`.sh`, commit messages, and inline comments. "It's just a
   draft" / "I'll fix it later" is how it ships.
2. **Secrets live outside the repo**, by design: the wrapper sources
   `~/.config/mcp-pg-readonly/env` (Unix) / `env.cmd` (Windows), or reads
   `PG_RO_PWD` from the process environment (e.g. `setx` on Windows). The
   repo never needs the real value to be useful — examples use `...` or
   `<placeholder>`. **The filled-in `anonymize.yaml` is a secret of the same
   class** (it contains the keywords being hidden): only
   `anonymize.example.yaml` with placeholders belongs in the repo;
   `anonymize.yaml` is gitignored and ACL-locked like `env.cmd`.
3. **Pre-commit gate.** Before `git commit`, scan the diff:
   `git diff --cached | grep -iE 'password|pwd|passwd|secret|token'`.
   Hits in lines that look like `= <real-value>` (not `<placeholder>` or
   `...`) → stop and replace. Also check `git diff --cached -- anonymize.yaml`
   is EMPTY (the file must never be staged). Keep this rule even if it feels
   paranoid.
4. **If a secret slips into a commit anyway**, do BOTH before pushing:
   - **Rotate the secret at the server** (`ALTER ROLE ... PASSWORD`). This
     is the only thing that actually closes the hole — history cleanup can
     never recall clones, forks, or GitHub orphan-URLs.
   - **Rewrite history before `git push`** (`git commit --amend` /
     `filter-branch` / `filter-repo`). Once pushed, the orphan is on
     GitHub for ~90 days regardless of force-push; recovery means deleting
     and recreating the repo, or opening a GitHub Support ticket.
5. **Verify leak cleanup by URL, not `git log`.** After a force-push,
   `git log -p` (and `git grep` on `origin/main`) show the clean history —
   but the leaked commit is still at
   `github.com/<owner>/<repo>/commit/<old-sha>`. Open that URL in a
   browser to confirm it 404s. Only then is the leak actually off GitHub.
