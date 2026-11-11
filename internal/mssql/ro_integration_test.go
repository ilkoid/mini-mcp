package mssql

import (
	"context"
	"database/sql"
	"mini-mcp/internal/result"
	"os"
	"strings"
	"testing"
	"time"

	"mini-mcp/internal/anon"

	_ "github.com/microsoft/go-mssqldb"
)

// Integration tests against a real SQL Server. Skipped unless
// MSSQL_TEST_DSN is set (a full sqlserver://… URL). NEVER point this at a
// working database — the probes are read-only, but the policy is to keep
// test batteries on scratch instances.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MSSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MSSQL_TEST_DSN not set; skipping integration tests")
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return db
}

func TestMSSQLReadOnly_Integration(t *testing.T) {
	db := testDB(t)
	a, err := anon.New([]anon.Rule{{
		ReadSubstring:  kw,
		WriteSubstring: alias,
		EqAny:          []string{"sampleword", alias},
	}})
	if err != nil {
		t.Fatal(err)
	}
	Anonymizer = a
	t.Cleanup(func() { Anonymizer = nil })

	ctx := context.Background()

	res, err := RunReadOnly(ctx, db, "SELECT CAST(1.23456 AS DECIMAL(10,2)) AS r, CAST(9223372036854775807 AS BIGINT) AS big, CAST(1 AS BIT) AS flag", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || len(res.Rows) != 1 || len(res.Rows[0]) != 3 {
		t.Fatalf("probe shape: %+v", res)
	}
	if res.Rows[0][0] != "1.23" {
		t.Fatalf("decimal must arrive as exact string, got %#v", res.Rows[0][0])
	}
	if res.Rows[0][1] != "9223372036854775807" {
		t.Fatalf("bigint must arrive as string, got %#v", res.Rows[0][1])
	}
	if res.Rows[0][2] != true {
		t.Fatalf("bit must arrive as bool, got %#v", res.Rows[0][2])
	}

	// N'' filter roundtrip: the alias restores to the keyword and expands to
	// every eq_any spelling (two-era data, Д-4/Д-5).
	fres, err := RunReadOnly(ctx, db,
		"SELECT count(*) AS n FROM (SELECT N'"+alias+"' AS brand) t WHERE brand = N'"+alias+"'", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fres.RowCount != 1 || len(fres.Rows) != 1 || asInt(fres.Rows[0][0]) != 1 {
		t.Fatalf("two-era N'' filter did not match: %+v", fres)
	}

	// Row cap: 5000-row probe must truncate at result.MaxRows (4000).
	cres, err := RunReadOnly(ctx, db,
		"SELECT TOP 5000 ROW_NUMBER() OVER (ORDER BY a.object_id, b.object_id) AS n FROM sys.all_objects a CROSS JOIN sys.all_objects b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cres.Truncated || cres.RowCount > result.MaxRows {
		t.Fatalf("row cap not engaged: truncated=%v rowCount=%d", cres.Truncated, cres.RowCount)
	}
	if cres.Notice == "" || !strings.Contains(cres.Notice, "truncated") {
		t.Fatalf("truncation notice missing: %q", cres.Notice)
	}
}

func asInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		var n int64
		for _, c := range x {
			if c < '0' || c > '9' {
				return -1
			}
			n = n*10 + int64(c-'0')
		}
		return n
	}
	return -1
}
