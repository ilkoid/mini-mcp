// mcp-pg-readonly is a tiny read-only PostgreSQL MCP server for the ZCode agent.
//
// It exposes three non-destructive tools (pg_readonly_query, pg_list_tables,
// pg_describe_table) over stdio JSON-RPC. Read-only is enforced at the
// database layer by the dedicated `mcp_ro` role (see role-bootstrap.sql), with
// a READ ONLY transaction + statement_timeout + output caps as belt-and-
// suspenders (see design doc §4). On top of that, every result is anonymized
// through a YAML rules file (--anon-config, REQUIRED): sensitive keywords are
// replaced with aliases on the way out, and the agent's alias values are
// translated back to the real keywords on the way in (inside this process —
// the session never observes the originals).
//
// Usage (stdio):
//
//	mcp-pg-readonly --database wb_data_test --anon-config anonymize.yaml
//
// Selftest (read-only: connects, runs SELECT probes, verifies the write
// precheck rejects writes WITHOUT sending any write statement to the server,
// and checks the role holds no non-SELECT table grants via the catalog):
//
//	mcp-pg-readonly --selftest --database wb_data_test --anon-config anonymize.yaml
//
// All logging goes to stderr — stdout carries ONLY MCP protocol frames. Any
// stray stdout byte breaks the stdio transport (ZCode shows toolCount=0).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"mini-mcp/internal/anon"
	"mini-mcp/internal/pg"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connection defaults mirror poncho-ai's BuildPgDSN (local dev RYZEN-ILKOID).
// Overridable via PGHOST / PGPORT / PGSSLMODE env vars. The database is
// REQUIRED (no silent prod default in the binary — design §3.3).
const (
	defaultHost    = "192.168.10.7"
	defaultPort    = "15432"
	defaultUser    = "mcp_ro"
	defaultPwdEnv  = "PG_RO_PWD" // distinct from PG_PWD (admin/app downloaders)
	defaultSSLMode = "disable"
	serverVersion  = "0.3.0"
)

// defaultAnonConfig is <home>\.config\mcp-pg-readonly\anonymize.yaml — the
// same directory the run.cmd wrapper uses for env.cmd. Computed at runtime
// (Windows %USERPROFILE% / Unix $HOME); empty string means "no home, must be
// passed explicitly".
func defaultAnonConfig() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "mcp-pg-readonly", "anonymize.yaml")
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("mcp-pg-readonly: ")
	// All logger output goes to stderr (stdout discipline — design §3.5).
	log.SetOutput(os.Stderr)

	var (
		database   = flag.String("database", "", "PostgreSQL database name (required; no silent prod default)")
		user       = flag.String("user", defaultUser, "PostgreSQL user (default: mcp_ro)")
		pwdEnvName = flag.String("password-env", defaultPwdEnv, "name of the env var holding the password (default: PG_RO_PWD)")
		selftest   = flag.Bool("selftest", false, "connect as the role, verify SELECT works and writes are rejected CLIENT-SIDE (no write is ever sent), then exit (prints to stderr)")
		anonConfig = flag.String("anon-config", defaultAnonConfig(), "path to the anonymization rules YAML (required, fail-closed; the file is a SECRET — it contains the keywords being hidden)")
		// Timeouts (design §4.2). Defaults live in the pg package; flags
		// override per server entry via config.json args.
		stmtTimeoutFlag  = flag.Duration("statement-timeout", pg.StatementTimeout, "per-statement server-side timeout, SET LOCAL inside the RO txn (default: 2m)")
		queryTimeoutFlag = flag.Duration("query-timeout", pg.QueryTimeout, "whole-call ctx deadline; must exceed statement-timeout (default: 3m)")
	)
	flag.Parse()

	// Apply to the package knobs BEFORE any query (selftest included).
	pg.StatementTimeout = *stmtTimeoutFlag
	pg.QueryTimeout = *queryTimeoutFlag
	if pg.QueryTimeout <= pg.StatementTimeout {
		fmt.Fprintf(os.Stderr, "error: --query-timeout (%s) must be greater than --statement-timeout (%s) so the DB-side cancel fires first\n", pg.QueryTimeout, pg.StatementTimeout)
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
	pg.Anonymizer = masker
	fmt.Fprintf(os.Stderr, "anonymizer: %d rule(s) loaded from %s\n", masker.Len(), *anonConfig)

	if *database == "" {
		// PGDATABASE override (libpq convention).
		if v := os.Getenv("PGDATABASE"); v != "" {
			*database = v
		}
	}
	if *database == "" {
		fmt.Fprintln(os.Stderr, "error: --database is required (or set PGDATABASE); refusing to pick a default")
		flag.Usage()
		os.Exit(2)
	}

	pwd := os.Getenv(*pwdEnvName)
	if pwd == "" {
		fmt.Fprintf(os.Stderr, "error: empty password: set %s env var (wrapper sources ~/.config/mcp-pg-readonly/env)\n", *pwdEnvName)
		os.Exit(2)
	}

	dsn := buildDSN(*user, pwd, *database)
	pool, err := pg.NewPool(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if *selftest {
		os.Exit(runSelftest(context.Background(), pool, *database))
	}

	// Serve MCP over stdio. From here on, NOTHING may write to stdout except
	// the SDK's protocol frames.
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-pg-readonly",
		Title:   "PostgreSQL read-only (anonymized)",
		Version: serverVersion,
	}, nil)
	pg.RegisterTools(server, pool, *user, *pwdEnvName)

	ctx := context.Background()
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "serve error: %v\n", err)
		os.Exit(1)
	}
}

// buildDSN constructs a libpq URL. Password is URL-encoded so special chars
// don't break parsing. sslmode defaults to disable (mirrors BuildPgDSN) and is
// overridable via PGSSLMODE.
func buildDSN(user, pwd, database string) string {
	host := envOr("PGHOST", defaultHost)
	port := envOr("PGPORT", defaultPort)
	sslmode := envOr("PGSSLMODE", defaultSSLMode)
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		url.PathEscape(user), url.QueryEscape(pwd),
		host, port, url.PathEscape(database), sslmode)
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
//   - SELECT 1 and the numeric probe exercise the happy path;
//   - the write probe is rejected by the CLIENT precheck (RejectUnsafeSQL) —
//     nothing leaves the process;
//   - the role's table grants are checked through the read-only catalog:
//     any non-SELECT privilege is a hard failure;
//   - the READ ONLY transaction mode is confirmed via SHOW;
//   - the row-cap probe is informational.
//
// It never touches prod implicitly — the operator chooses the database via
// --database.
func runSelftest(ctx context.Context, pool *pgxpool.Pool, database string) int {
	fmt.Fprintf(os.Stderr, "selftest: database=%s\n", database)

	res, err := pg.RunReadOnly(ctx, pool, "SELECT 1 AS ok", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: SELECT 1 -> %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "ok: SELECT 1 -> %d row(s)\n", res.RowCount)

	// Numeric-encoding probe: pgtype.Numeric must render as an exact decimal
	// string, not fail the query — a live bug let every computed numeric
	// (round(), numeric division, ::numeric) error with "unexpected type".
	numRes, nerr := pg.RunReadOnly(ctx, pool, "SELECT round(1.23456::numeric, 2) AS r", nil)
	if nerr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: numeric probe -> %v\n", nerr)
		return 1
	}
	if numRes.RowCount != 1 || len(numRes.Rows) != 1 || len(numRes.Rows[0]) != 1 || numRes.Rows[0][0] != "1.23" {
		fmt.Fprintf(os.Stderr, "FAIL: numeric probe: got %v, want [[\"1.23\"]]\n", numRes.Rows)
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: numeric probe -> round(numeric) as exact string")

	// Case-preserving masking probe (Д-6, leak-free): pass each rule's own
	// ALIAS as a bind arg through the full live pipeline (arg Unmap ->
	// DB upper() -> output masking) and report the returned SHAPE plus the
	// keyword's case shape (booleans only — never rule values). Numeric
	// rules legitimately return canonical (digits have no case).
	for i, alias := range pg.Anonymizer.ProbeAliases() {
		shapeRes, serr := pg.RunReadOnly(ctx, pool,
			"SELECT upper($1) AS u, ($1 = lower($1)) AS kw_lower, ($1 = upper($1)) AS kw_upper, ($1 ~ '[A-Z]') AS has_up, ($1 ~ '[a-z]') AS has_lo",
			[]any{alias})
		if serr != nil {
			fmt.Fprintf(os.Stderr, "note: upper probe rule #%d skipped: %v\n", i+1, serr)
			continue
		}
		if shapeRes.RowCount != 1 || len(shapeRes.Rows) != 1 || len(shapeRes.Rows[0]) != 5 {
			continue
		}
		got, _ := shapeRes.Rows[0][0].(string)
		kwLower, _ := shapeRes.Rows[0][1].(bool)
		kwUpper, _ := shapeRes.Rows[0][2].(bool)
		hasUp, _ := shapeRes.Rows[0][3].(bool)
		hasLo, _ := shapeRes.Rows[0][4].(bool)
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
	// statement before anything is sent to the server. A write statement is
	// never issued against the database, per the no-writes policy.
	const writeProbe = "CREATE TEMP TABLE _selftest_write_probe (id int)"
	if werr := pg.RejectUnsafeSQL(writeProbe); werr == nil {
		fmt.Fprintln(os.Stderr, "FAIL: write probe passed the client precheck — a write could reach the server")
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: write probe rejected by CLIENT precheck (no write statement was sent to the server)")

	// Anonymization-guard probe — CLIENT-SIDE ONLY: leak functions (hash /
	// substring / length families) must be rejected before execution
	// (test report 2026-09-03, Д-1/Д-2/Д-3).
	const guardProbe = "SELECT md5(brand_name) AS h FROM sales LIMIT 1"
	if gerr := pg.RejectUnsafeSQL(guardProbe); gerr == nil {
		fmt.Fprintln(os.Stderr, "FAIL: md5 probe passed the client precheck — a derivative leak could reach the output")
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: anonymization guard rejects leak functions CLIENT-side (md5 probe, nothing sent)")

	// Grants check via the read-only catalog: the role must hold no
	// non-SELECT table privileges. (The role + RO txn remain the real
	// boundary; this makes a mis-granted role visible.)
	grantRes, gerr := pg.RunReadOnly(ctx, pool, `
		SELECT count(*)::text AS bad_grants
		FROM information_schema.role_table_grants
		WHERE grantee = current_user AND privilege_type NOT IN ('SELECT')`, nil)
	if gerr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: grants check -> %v\n", gerr)
		return 1
	}
	if grantRes.RowCount != 1 || len(grantRes.Rows) != 1 || grantRes.Rows[0][0] != "0" {
		fmt.Fprintf(os.Stderr, "FAIL: role holds non-SELECT table grants: %v\n", grantRes.Rows)
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: role holds no non-SELECT table grants (catalog)")

	// READ ONLY transaction mode.
	roRes, rerr := pg.RunReadOnly(ctx, pool, "SHOW transaction_read_only", nil)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: SHOW transaction_read_only -> %v\n", rerr)
		return 1
	}
	if roRes.RowCount != 1 || len(roRes.Rows) != 1 || roRes.Rows[0][0] != "on" {
		fmt.Fprintf(os.Stderr, "FAIL: transaction_read_only is not 'on': %v\n", roRes.Rows)
		return 1
	}
	fmt.Fprintln(os.Stderr, "ok: queries run inside a READ ONLY transaction")

	// Optional row-cap probe — informational, not pass/fail.
	capRes, _ := pg.RunReadOnly(ctx, pool, "SELECT i FROM generate_series(1, 5000) AS t(i)", nil)
	if capRes != nil && capRes.Truncated {
		fmt.Fprintf(os.Stderr, "ok: row cap engaged at %d rows (truncated=true)\n", capRes.RowCount)
	}

	fmt.Fprintf(os.Stderr, "ok: anonymizer active (%d rule(s))\n", pg.Anonymizer.Len())
	fmt.Fprintln(os.Stderr, "selftest: PASS")
	return 0
}
