// mcp-pg-readonly is a tiny read-only PostgreSQL MCP server for the ZCode agent.
//
// It exposes three non-destructive tools (pg_readonly_query, pg_list_tables,
// pg_describe_table) over stdio JSON-RPC. Read-only is enforced at the
// database layer by the dedicated `mcp_ro` role (see role-bootstrap.sql), with
// a READ ONLY transaction + statement_timeout + output caps as belt-and-
// suspenders (see design doc §4).
//
// Usage (stdio):
//
//	mcp-pg-readonly --database wb_data_test
//
// Selftest (connects, verifies read-only role rejects writes):
//
//	mcp-pg-readonly --selftest --database wb_data_test
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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connection defaults mirror poncho-ai's BuildPgDSN (local dev RYZEN-ILKOID).
// Overridable via PGHOST / PGPORT / PGSSLMODE env vars. The database is
// REQUIRED (no silent prod default in the binary — design §3.3).
const (
	defaultHost = "192.168.10.7"
	defaultPort = "15432"
	defaultUser = "mcp_ro"
	defaultPwdEnv = "PG_RO_PWD" // distinct from PG_PWD (admin/app downloaders)
	defaultSSLMode = "disable"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("mcp-pg-readonly: ")
	// All logger output goes to stderr (stdout discipline — design §3.5).
	log.SetOutput(os.Stderr)

	var (
		database   = flag.String("database", "", "PostgreSQL database name (required; no silent prod default)")
		user       = flag.String("user", defaultUser, "PostgreSQL user (default: mcp_ro)")
		pwdEnvName = flag.String("password-env", defaultPwdEnv, "name of the env var holding the password (default: PG_RO_PWD)")
		selftest   = flag.Bool("selftest", false, "connect as the role, verify SELECT works and writes are rejected, then exit (prints to stderr)")
	)
	flag.Parse()

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
	pool, err := newPool(dsn)
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
		Title:   "PostgreSQL read-only",
		Version: "0.1.0",
	}, nil)
	registerTools(server, pool)

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

// newPool builds a narrow pgxpool. MaxConns=1 per stdio process — tool calls
// in a session are serial, and a larger pool would starve other concurrent
// sessions sharing the role's CONNECTION LIMIT (design §3.4, §4.5).
func newPool(dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	// statement_timeout is set per-query (SET LOCAL) inside runReadOnly, NOT
	// globally here — see design §3.4 (poncho-ai's pool AfterConnect bulk
	// params are harmful for this use case).

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// runSelftest verifies the read-only boundary end-to-end: SELECT works, writes
// fail. Prints a human-readable report to stderr and returns the exit code.
// It never touches prod implicitly — the operator chooses the database via
// --database.
func runSelftest(ctx context.Context, pool *pgxpool.Pool, database string) int {
	fmt.Fprintf(os.Stderr, "selftest: database=%s\n", database)

	res, err := runReadOnly(ctx, pool, "SELECT 1 AS ok", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: SELECT 1 -> %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "ok: SELECT 1 -> %d row(s)\n", res.RowCount)

	// Write probe. The role + READ ONLY txn must reject this. Any "success"
	// here is a hard failure of the security boundary.
	_, werr := runReadOnly(ctx, pool, "CREATE TEMP TABLE _selftest_write_probe (id int)", nil)
	if werr == nil {
		fmt.Fprintln(os.Stderr, "FAIL: write probe succeeded — read-only boundary is BROKEN")
		return 1
	}
	fmt.Fprintf(os.Stderr, "ok: write probe rejected as expected (%v)\n", werr)

	// Optional row-cap probe — informational, not pass/fail.
	capRes, _ := runReadOnly(ctx, pool, "SELECT i FROM generate_series(1, 600) AS t(i)", nil)
	if capRes != nil && capRes.Truncated {
		fmt.Fprintf(os.Stderr, "ok: row cap engaged at %d rows (truncated=true)\n", capRes.RowCount)
	}

	fmt.Fprintln(os.Stderr, "selftest: PASS")
	return 0
}
