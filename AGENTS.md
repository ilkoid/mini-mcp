# AGENTS.md — mcp-pg-readonly

A tiny **read-only PostgreSQL MCP server** for the ZCode agent. Stdio JSON-RPC,
three tools: `pg_readonly_query`, `pg_list_tables`, `pg_describe_table`.
Separate Go module (`github.com/ilkoid/mcp-pg-readonly`) — **not** part of
poncho-ai. The folder is `mini-mcp`; the module/binary name is `mcp-pg-readonly`
deliberately, so the user-scope MCP `command` path stays stable across repo
renames. Full design: `/Users/ilkoid/dev/poncho-ai/reports/2026-07-25_mcp-pg-readonly-design.md`.

## Build, test, run

```bash
go build -o ~/bin/mcp-pg-readonly .                       # build + install
go test ./...                                             # unit (no DB needed)
PG_TEST_DSN="postgres://mcp_ro:...@host:port/db?sslmode=disable" \
  go test -run TestReadOnly -v ./...                      # integration (skipped w/o DSN)
~/bin/mcp-pg-readonly --selftest --database wb_data_test  # end-to-end RO boundary check
```

- `--database` is **required** (no silent prod default in the binary).
- **Never** point `PG_TEST_DSN` at prod.
- No separate lint/typecheck script; `go build`/`go vet` are the gate.

## Files

| File | Role |
|---|---|
| `main.go` | Entry, flags, DSN, pool, `runSelftest`. |
| `tools.go` | MCP tool registration, shared annotations, `describeTable`. |
| `ro.go` | **Security core**: `runReadOnly`, `rejectUnsafeSQL`, output caps. |
| `encode.go` | PG→JSON type mapping (int8/numeric as strings, etc.). |
| `role-bootstrap.sql` | Phase A — creates the `mcp_ro` role (the real boundary). |
| `role-bootstrap-layers.sql` | Phase B placeholder (`analytical`/`recommendation` schemas don't exist yet — intentionally inert). |
| `mcp-pg-readonly-run.sh` | Secret wrapper installed at `~/bin/mcp-pg-readonly-run`; sources `~/.config/mcp-pg-readonly/env` (path overridable via `MCP_PG_SECRETS`), then execs the binary. |
| `*_test.go` | Unit (`ro_test.go`, `tools_test.go`) + integration (`ro_integration_test.go`, needs `PG_TEST_DSN`). |

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

4. **Single-statement SQL only.** `rejectUnsafeSQL` rejects empty, multi
   statement (any `;` other than one optional trailing), and leading non-read
   keywords. Agents must pass values via `$1,$2` params, not inline. Keep this
   rule when editing.

5. **Fixed result envelope.** `Result{columns, rows, row_count, truncated,
   notice}` is the contract with the agent. Caps: `maxRows=500`,
   `maxBytes=64KB`. Double truncation (server cap + ZCode client budget) is
   intentional; keep server caps ≤ client budget.

6. **Pool discipline.** `MaxConns = 1` per stdio process (tool calls are
   serial). Role `CONNECTION LIMIT = 8` covers several sessions + inspector +
   selftest. If you see `too many connections for role "mcp_ro"`, **raise the
   role limit** — do **not** bump pool `MaxConns` (starves other sessions).
   `statement_timeout` is set per-query via `set_config(..., true)` (NOT
   globally in `newPool`, and NOT via `SET LOCAL $1` which PG doesn't support).

## JSON type encoding (`encode.go`)

- **int8 / numeric → JSON string** (WB `nm_id` exceeds 2^53; float64 would
  corrupt it). Guarded by `TestEncodeInt8AsString`.
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
path string (not an array). Prod and test are different server names
(`pg-readonly` / `pg-readonly-test`) so a selftest can't hit prod. Password
stays out of that JSON — the wrapper sources it.
