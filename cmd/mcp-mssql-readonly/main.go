// mcp-mssql-readonly is a tiny read-only SQL Server MCP server for the ZCode
// agent — the MSSQL sibling of mcp-pg-readonly.
//
// It exposes four non-destructive tools (mssql_readonly_query,
// mssql_list_dbs, mssql_list_tables, mssql_describe_table) over stdio
// JSON-RPC. Read-only is enforced at the database layer by the dedicated
// SELECT-only role (default ai_reader), with a single-statement precheck +
// context timeouts + output caps as belt-and-suspenders. SQL Server has no
// READ ONLY transaction mode, so the role ACLs are the write boundary;
// --selftest verifies the grants through sys.database_permissions. On top of
// that, every result is anonymized through the SAME YAML rules file as the
// PG sibling (--anon-config, REQUIRED, fail-closed): sensitive keywords are
// replaced with aliases on the way out, and the agent's alias values are
// translated back to the real keywords on the way in (inside this process —
// the session never observes the originals).
//
// Usage (stdio):
//
//	mcp-mssql-readonly --database DWH_MART --anon-config anonymize.yaml
//
// Selftest (read-only: connects, runs SELECT probes, verifies writes are
// rejected CLIENT-SIDE without sending any write statement to the server,
// and checks the role holds no write grants/role memberships via the
// catalog):
//
//	mcp-mssql-readonly --selftest --database DWH_MART --anon-config anonymize.yaml
//
// All logging goes to stderr — stdout carries ONLY MCP protocol frames. Any
// stray stdout byte breaks the stdio transport (ZCode shows toolCount=0).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"mini-mcp/internal/anon"
	"mini-mcp/internal/mssql"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connection defaults for the shared-VPS deployment (mirrors the pg VPS
// wrapper): the DWH endpoint, overridable via MSSQL_HOST / MSSQL_PORT /
// MSSQL_ENCRYPT env vars. The database is REQUIRED (no silent default in
// the binary).
const (
	defaultHost    = "10.120.11.233"
	defaultPort    = "1433"
	defaultUser    = "ai_reader"
	defaultPwdEnv  = "MSSQL_AI_PWD"
	defaultEncrypt = "disable"
	serverVersion  = "0.1.1"
)

// defaultAnonConfig is <home>\.config\mssql-mssql-readonly\anonymize.yaml.
// The deployment typically overrides it with --anon-config pointing at the
// SHARED rules file (E:\ARM_AI\.config\mcp-pg-readonly\anonymize.yaml) so
// both servers mask the same keywords.
func defaultAnonConfig() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "mcp-mssql-readonly", "anonymize.yaml")
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("mcp-mssql-readonly: ")
	// All logger output goes to stderr (stdout discipline).
	log.SetOutput(os.Stderr)

	var (
		database   = flag.String("database", "", "SQL Server database name (required; no silent default)")
		user       = flag.String("user", defaultUser, "SQL Server user (default: ai_reader)")
		pwdEnvName = flag.String("password-env", defaultPwdEnv, "name of the env var holding the password (default: MSSQL_AI_PWD)")
		selftest   = flag.Bool("selftest", false, "connect as the role, verify SELECT works and writes are rejected CLIENT-SIDE (no write is ever sent), then exit (prints to stderr)")
		anonConfig = flag.String("anon-config", defaultAnonConfig(), "path to the anonymization rules YAML (required, fail-closed; the file is a SECRET — it contains the keywords being hidden)")
		encrypt    = flag.String("encrypt", envOr("MSSQL_ENCRYPT", defaultEncrypt), "TDS encryption: disable | false | true | strict (default: disable)")
		// Timeouts. Defaults live in the mssql package; flags override per
		// server entry via config.json args.
		stmtTimeoutFlag  = flag.Duration("statement-timeout", mssql.StatementTimeout, "per-statement context timeout, cancels the in-flight query first (default: 2m)")
		queryTimeoutFlag = flag.Duration("query-timeout", mssql.QueryTimeout, "whole-call ctx deadline; must exceed statement-timeout (default: 3m)")
	)
	flag.Parse()

	// Apply to the package knobs BEFORE any query (selftest included).
	mssql.StatementTimeout = *stmtTimeoutFlag
	mssql.QueryTimeout = *queryTimeoutFlag
	if mssql.QueryTimeout <= mssql.StatementTimeout {
		fmt.Fprintf(os.Stderr, "error: --query-timeout (%s) must be greater than --statement-timeout (%s) so the statement-side cancel fires first\n", mssql.QueryTimeout, mssql.StatementTimeout)
		os.Exit(2)
	}

	// Anonymizer loads before anything else: fail closed on a missing/invalid
	// rules file. An empty ruleset (`rules: []`) is a deliberate, visible
	// choice for a no-masking instance.
	masker, err := anon.Load(*anonConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	mssql.Anonymizer = masker
	fmt.Fprintf(os.Stderr, "anonymizer: %d rule(s) loaded from %s\n", masker.Len(), *anonConfig)

	if *database == "" {
		if v := os.Getenv("MSSQL_DATABASE"); v != "" {
			*database = v
		}
	}
	if *database == "" {
		fmt.Fprintln(os.Stderr, "error: --database is required (or set MSSQL_DATABASE); refusing to pick a default")
		flag.Usage()
		os.Exit(2)
	}

	pwd := os.Getenv(*pwdEnvName)
	if pwd == "" {
		fmt.Fprintf(os.Stderr, "error: empty password: set %s env var (wrapper sources the env.cmd secrets file)\n", *pwdEnvName)
		os.Exit(2)
	}

	db, err := mssql.NewDB(mssql.Config{
		Host:     envOr("MSSQL_HOST", defaultHost),
		Port:     envOr("MSSQL_PORT", defaultPort),
		User:     *user,
		Password: pwd,
		Database: *database,
		Encrypt:  *encrypt,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connect: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if *selftest {
		os.Exit(runSelftest(context.Background(), db, *database))
	}

	// Serve MCP over stdio. From here on, NOTHING may write to stdout except
	// the SDK's protocol frames.
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-mssql-readonly",
		Title:   "SQL Server read-only (anonymized)",
		Version: serverVersion,
	}, nil)
	mssql.RegisterTools(server, db, *user, *pwdEnvName)

	ctx := context.Background()
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "serve error: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// runSelftest verifies the read-only posture WITHOUT sending a single write
// statement to the server (policy: working databases are never written to,
// not even by "expected to fail" probes):
//
//   - SELECT 1 and the decimal/bigint probes exercise the happy path;
//   - the write/hashbytes/varbinary probes are rejected by the CLIENT
//     precheck (RejectUnsafeSQL) — nothing leaves the process;
//   - the role's permissions are checked through the read-only catalog:
//     any non-SELECT object grant, dangerous database grant, or membership
//     in a writer role is a hard failure (SQL Server has no READ ONLY txn,
//     so the grants ARE the boundary here);
//   - the row-cap probe is informational.
func runSelftest(ctx context.Context, db *sql.DB, database string) int {
	fmt.Fprintf(os.Stderr, "selftest: database=%s\n", database)

	res, err := mssql.RunReadOnly(ctx, db, "SELECT 1 AS ok", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: SELECT 1 -> %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "ok: SELECT 1 -> %d row(s)\n", res.RowCount)

	// Decimal-encoding probe: DECIMAL must render as the exact decimal
	// string, not lose digits through float64.
	numRes, nerr := mssql.RunReadOnly(ctx, db, "SELECT CAST(1.23456 AS DECIMAL(10,2)) AS r", nil)
	if nerr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: decimal probe -> %v\n", nerr)
		return 1
	}
	if numRes.RowCount != 1 || len(numRes.Rows) != 1 || len(numRes.Rows[0]) != 1 || numRes.Rows[0][0] != "1.23" {
		fmt.Fprintf(os.Stderr, "FAIL: decimal probe: got %v, want [[\"1.23\"]]\n", numRes.Rows)
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: decimal probe -> exact string")

	// BIGINT probe: int64 must arrive as a JSON string (2^53 safety).
	bigRes, berr := mssql.RunReadOnly(ctx, db, "SELECT CAST(9223372036854775807 AS BIGINT) AS big", nil)
	if berr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: bigint probe -> %v\n", berr)
		return 1
	}
	if bigRes.RowCount != 1 || len(bigRes.Rows[0]) != 1 || bigRes.Rows[0][0] != "9223372036854775807" {
		fmt.Fprintf(os.Stderr, "FAIL: bigint probe: got %v, want [[\"9223372036854775807\"]]\n", bigRes.Rows)
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: bigint probe -> JSON string")

	// Case-preserving masking probe (Д-6, leak-free): pass each rule's own
	// ALIAS as a bind arg through the full live pipeline (arg Unmap ->
	// DB UPPER() -> output masking) and report the returned SHAPE plus the
	// keyword's case shape (booleans only — never rule values). The CS_AS
	// collation makes '='/LIKE case-sensitive so the shape flags are real.
	// Latin1 collation only senses Latin letters — Cyrillic keywords report
	// has_up/has_lo=false; the upper_result column still covers them.
	for i, alias := range mssql.Anonymizer.ProbeAliases() {
		shapeRes, serr := mssql.RunReadOnly(ctx, db,
			`SELECT UPPER(@p1) AS u,
			        CASE WHEN @p1 COLLATE Latin1_General_CS_AS = LOWER(@p1) THEN 1 ELSE 0 END AS kw_lower,
			        CASE WHEN @p1 COLLATE Latin1_General_CS_AS = UPPER(@p1) THEN 1 ELSE 0 END AS kw_upper,
			        CASE WHEN @p1 COLLATE Latin1_General_CS_AS LIKE '%[A-Z]%' THEN 1 ELSE 0 END AS has_up,
			        CASE WHEN @p1 COLLATE Latin1_General_CS_AS LIKE '%[a-z]%' THEN 1 ELSE 0 END AS has_lo`,
			[]any{alias})
		if serr != nil {
			fmt.Fprintf(os.Stderr, "note: upper probe rule #%d skipped: %v\n", i+1, serr)
			continue
		}
		if shapeRes.RowCount != 1 || len(shapeRes.Rows) != 1 || len(shapeRes.Rows[0]) != 5 {
			continue
		}
		got, _ := shapeRes.Rows[0][0].(string)
		kwLower := flagInt64(shapeRes.Rows[0][1]) == 1
		kwUpper := flagInt64(shapeRes.Rows[0][2]) == 1
		hasUp := flagInt64(shapeRes.Rows[0][3]) == 1
		hasLo := flagInt64(shapeRes.Rows[0][4]) == 1
		shape := "unexpected"
		switch got {
		case strings.ToUpper(alias):
			shape = "uppercase-alias"
		case alias:
			shape = "canonical"
		}
		fmt.Fprintf(os.Stderr, "probe rule #%d: alias_len=%d upper_result=%s keyword{all_lower=%v all_upper=%v has_upper=%v has_lower=%v}\n",
			i+1, len(alias), shape, kwLower, kwUpper, hasUp, hasLo)
	}

	// Write probe — CLIENT-SIDE ONLY. RejectUnsafeSQL must refuse the
	// statement before anything is sent to the server.
	const writeProbe = "CREATE TABLE _selftest_write_probe (id int)"
	if werr := mssql.RejectUnsafeSQL(writeProbe); werr == nil {
		fmt.Fprintln(os.Stderr, "FAIL: write probe passed the client precheck — a write could reach the server")
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: write probe rejected by CLIENT precheck (no write statement was sent to the server)")

	// Anonymization-guard probes — CLIENT-SIDE ONLY: leak functions and the
	// binary-cast pattern must be rejected before execution (test report
	// 2026-09-03, Д-1/Д-2/Д-3).
	const guardProbe = "SELECT HASHBYTES('MD5', brand_name) AS h FROM sales"
	if gerr := mssql.RejectUnsafeSQL(guardProbe); gerr == nil {
		fmt.Fprintln(os.Stderr, "FAIL: HASHBYTES probe passed the client precheck — a derivative leak could reach the output")
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: anonymization guard rejects leak functions CLIENT-side (HASHBYTES probe, nothing sent)")

	const castProbe = "SELECT CAST(brand AS VARBINARY(100)) AS b FROM cards"
	if gerr := mssql.RejectUnsafeSQL(castProbe); gerr == nil {
		fmt.Fprintln(os.Stderr, "FAIL: VARBINARY cast probe passed the client precheck — an encoding leak could reach the output")
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: anonymization guard rejects binary casts CLIENT-side (VARBINARY probe, nothing sent)")

	// Grants check via the read-only catalog: the role must hold no write
	// privilege and no writer-role membership (SQL Server has no READ ONLY
	// txn — this IS the boundary verification).
	grantRes, gerr := mssql.RunReadOnly(ctx, db, `
		SELECT
		  (SELECT count(*) FROM sys.database_permissions pe
		     JOIN sys.database_principals pr ON pe.grantee_principal_id = pr.principal_id
		     WHERE pr.name = CURRENT_USER AND pe.state IN ('G','W')
		       AND ((pe.class_desc = 'OBJECT_OR_COLUMN' AND pe.permission_name <> 'SELECT')
		         OR (pe.class_desc = 'DATABASE' AND pe.permission_name NOT IN ('SELECT','VIEW DEFINITION','CONNECT','SHOWPLAN'))))
		  + (CASE WHEN IS_ROLEMEMBER('db_datawriter') = 1 THEN 1 ELSE 0 END)
		  + (CASE WHEN IS_ROLEMEMBER('db_owner') = 1 THEN 1 ELSE 0 END)
		  + (CASE WHEN IS_ROLEMEMBER('db_ddladmin') = 1 THEN 1 ELSE 0 END)
		  AS bad_grants`, nil)
	if gerr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: grants check -> %v\n", gerr)
		return 1
	}
	if grantRes.RowCount != 1 || len(grantRes.Rows) != 1 || flagInt64(grantRes.Rows[0][0]) != 0 {
		fmt.Fprintf(os.Stderr, "FAIL: role holds write grants or writer-role membership: %v\n", grantRes.Rows)
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: role holds no write grants / writer-role memberships (catalog)")

	// Optional row-cap probe — informational, not pass/fail. The self-cross-
	// join guarantees >= 5000 rows even in a near-empty database (the probe
	// must exceed the 4000-row MaxRows cap to engage it).
	capRes, _ := mssql.RunReadOnly(ctx, db, `
		SELECT TOP 5000 ROW_NUMBER() OVER (ORDER BY a.object_id, b.object_id) AS n
		FROM sys.all_objects a CROSS JOIN sys.all_objects b`, nil)
	if capRes != nil && capRes.Truncated {
		fmt.Fprintf(os.Stderr, "ok: row cap engaged at %d rows (truncated=true)\n", capRes.RowCount)
	}

	fmt.Fprintf(os.Stderr, "ok: anonymizer active (%d rule(s))\n", mssql.Anonymizer.Len())
	fmt.Fprintln(os.Stderr, "selftest: PASS")
	return 0
}

// flagInt64 pulls an int64-ish cell out of an encoded row.
func flagInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return -1
}
