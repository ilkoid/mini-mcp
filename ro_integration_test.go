package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// jsonMarshal is a tiny indirection so encode tests don't repeat
// encoding/json import boilerplate.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Integration tests below require a live PostgreSQL with the mcp_ro role
// bootstrapped. They are skipped unless PG_TEST_DSN is set. NEVER point this
// at prod (design §8.1).
//
// Example:
//   PG_TEST_DSN="postgres://mcp_ro:SECRET@192.168.10.7:15432/wb_data_test?sslmode=disable" \
//     go test -run TestReadOnly -v

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test")
	}
	pool, err := newPool(dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestReadOnly_AllowsSelect(t *testing.T) {
	pool := testPool(t)
	res, err := runReadOnly(context.Background(), pool, "SELECT 1 AS ok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 {
		t.Fatalf("RowCount=%d want 1", res.RowCount)
	}
}

func TestReadOnly_RejectsWrite(t *testing.T) {
	pool := testPool(t)
	// Either the precheck (DX) or the role/txn must reject this.
	_, err := runReadOnly(context.Background(), pool, "CREATE TEMP TABLE _t (id int)", nil)
	if err == nil {
		t.Fatal("write unexpectedly succeeded — read-only boundary broken")
	}
}

func TestReadOnly_RowCap(t *testing.T) {
	pool := testPool(t)
	res, err := runReadOnly(context.Background(), pool, "SELECT i FROM generate_series(1, 600) AS t(i)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatalf("expected truncated=true; got RowCount=%d", res.RowCount)
	}
	if res.RowCount > maxRows {
		t.Fatalf("RowCount=%d exceeds maxRows=%d", res.RowCount, maxRows)
	}
}

func TestReadOnly_Int8JSONAsString(t *testing.T) {
	pool := testPool(t)
	// 2^53+1 must come back as a JSON string.
	res, err := runReadOnly(context.Background(), pool, "SELECT 9007199254740993::bigint AS nm", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("unexpected shape %+v", res)
	}
	b, err := json.Marshal(res.Rows[0][0])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"9007199254740993"` {
		t.Fatalf("int8 not string on wire: %s", b)
	}
}

func TestReadOnly_StatementTimeout(t *testing.T) {
	pool := testPool(t)
	// pg_sleep(35) exceeds the 30s statement_timeout → must error.
	_, err := runReadOnly(context.Background(), pool, "SELECT pg_sleep(35)", nil)
	if err == nil {
		t.Fatal("expected statement_timeout error, got nil")
	}
}

func TestReadOnly_BindsParams(t *testing.T) {
	pool := testPool(t)
	res, err := runReadOnly(context.Background(), pool, "SELECT $1::int AS a, $2::text AS b",
		[]any{42, "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 {
		t.Fatalf("RowCount=%d", res.RowCount)
	}
}
