package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRejectUnsafeSQL_Empty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t"} {
		if err := rejectUnsafeSQL(in); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}

func TestRejectUnsafeSQL_MultiStatement(t *testing.T) {
	cases := []string{
		"SELECT 1; SELECT 2",
		"SELECT 1;SELECT 2",
		"SELECT 1; -- comment\nSELECT 2;",
	}
	for _, in := range cases {
		if err := rejectUnsafeSQL(in); err == nil {
			t.Fatalf("expected multi-statement rejection for %q", in)
		}
	}
}

func TestRejectUnsafeSQL_TrailingSemicolonOK(t *testing.T) {
	for _, in := range []string{"SELECT 1", "SELECT 1;", "  SELECT 1 ;  ", "SELECT $1"} {
		if err := rejectUnsafeSQL(in); err != nil {
			t.Fatalf("expected OK for %q, got %v", in, err)
		}
	}
}

func TestRejectUnsafeSQL_NonReadKeywords(t *testing.T) {
	cases := []string{
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET x=1",
		"DELETE FROM t",
		"TRUNCATE t",
		"DROP TABLE t",
		"CREATE TABLE t (id int)",
		"ALTER TABLE t ADD COLUMN x int",
		"GRANT SELECT ON t TO x",
		"REVOKE ALL ON t FROM x",
		"COPY t FROM '/tmp/x'",
		"CALL my_proc()",
		"DO $$ BEGIN END $$",
		"VACUUM t",
		"MERGE INTO t USING s ON true WHEN MATCHED THEN UPDATE SET x=1",
	}
	for _, in := range cases {
		if err := rejectUnsafeSQL(in); err == nil {
			t.Fatalf("expected rejection for %q", in)
		}
	}
}

func TestRejectUnsafeSQL_ReadStatementsOK(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"SELECT * FROM sales WHERE nm_id = $1",
		"WITH cte AS (SELECT 1) SELECT * FROM cte",
		"SHOW timezone",
		"EXPLAIN SELECT 1",
		"(SELECT 1)", // parenthesized still classifies as SELECT
		"select 1",   // lowercase ok
	}
	for _, in := range cases {
		if err := rejectUnsafeSQL(in); err != nil {
			t.Fatalf("expected OK for %q, got %v", in, err)
		}
	}
}

func TestLeadingKeyword(t *testing.T) {
	cases := map[string]string{
		"SELECT 1":                "SELECT",
		"  select 1":              "SELECT",
		"(SELECT 1)":              "SELECT",
		"WITH x AS (SELECT 1)":    "WITH",
		"insert into t":           "INSERT",
		"  \t UPDATE t":           "UPDATE",
	}
	for in, want := range cases {
		if got := leadingKeyword(in); got != want {
			t.Fatalf("leadingKeyword(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWrapQueryError(t *testing.T) {
	// 57014 = query_canceled by statement_timeout -> actionable hint.
	stmtTO := &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
	err := wrapQueryError(stmtTO)
	msg := err.Error()
	if !errors.Is(err, stmtTO) {
		t.Fatal("57014 must remain wrap-compatible with the original PgError")
	}
	if want := fmt.Sprintf("query exceeded statement_timeout (%s)", statementTimeout); !strings.Contains(msg, want) {
		t.Fatalf("57014 hint missing %q: got %q", want, msg)
	}

	// Raw ctx deadline -> query-timeout hint.
	ctxErr := wrapQueryError(context.DeadlineExceeded)
	if want := fmt.Sprintf("query exceeded query-timeout (%s)", queryTimeout); !strings.Contains(ctxErr.Error(), want) {
		t.Fatalf("ctx hint missing %q: got %q", want, ctxErr.Error())
	}

	// Ordinary DB errors pass through unchanged.
	other := &pgconn.PgError{Code: "42P01", Message: `relation "x" does not exist`}
	if got := wrapQueryError(other); got != error(other) {
		t.Fatalf("non-timeout error must pass through: got %v", got)
	}
}

func TestEncodeInt8AsString(t *testing.T) {
	// WB nm_id values exceed 2^53 — they MUST come back as a JSON string,
	// not a number, or the agent corrupts the ID.
	big := int64(9_007_199_254_740_993) // 2^53 + 1
	e := newRowEncoder([]uint32{oidInt8})
	row, _, err := e.encode([]any{big})
	if err != nil {
		t.Fatal(err)
	}
	b, err := jsonMarshal(row[0])
	if err != nil {
		t.Fatal(err)
	}
	// Must be a quoted string, not a bare number.
	if s := string(b); s != `"9007199254740993"` {
		t.Fatalf("int8 not encoded as string: got %s", s)
	}
}

func TestEncodeNumericAsString(t *testing.T) {
	e := newRowEncoder([]uint32{oidNumeric})
	// numeric coming back as the string form "12345678901234567890.12"
	row, _, err := e.encode([]any{"12345678901234567890.12"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := jsonMarshal(row[0])
	if s := string(b); s != `"12345678901234567890.12"` {
		t.Fatalf("numeric not encoded as string: got %s", s)
	}
}

func TestEncodeNull(t *testing.T) {
	e := newRowEncoder([]uint32{oidInt8})
	row, _, err := e.encode([]any{nil})
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != nil {
		t.Fatalf("nil must encode to nil (-> null), got %v", row[0])
	}
}

func TestEncodeTimestampAsRFC3339(t *testing.T) {
	e := newRowEncoder([]uint32{oidTimestamptz})
	ts := mustTime("2026-07-25T10:00:00Z")
	row, _, err := e.encode([]any{ts})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := jsonMarshal(row[0])
	if s := string(b); s != `"2026-07-25T10:00:00Z"` {
		t.Fatalf("timestamp not RFC3339 UTC: got %s", s)
	}
}

func TestEncodeRejectsLargeBytea(t *testing.T) {
	e := newRowEncoder([]uint32{oidBytea})
	big := make([]byte, 2048)
	if _, _, err := e.encode([]any{big}); err == nil {
		t.Fatal("expected error for oversized bytea")
	}
}

func TestLooksNumeric(t *testing.T) {
	good := []string{"123", "-123", "+1.5", "1e10", "1.5e-3", ".5", "-.5", "123.456"}
	for _, s := range good {
		if !looksNumeric(s) {
			t.Errorf("looksNumeric(%q)=false, want true", s)
		}
	}
	bad := []string{"", "abc", "1.2.3", "--1", "1e", "12 34"}
	for _, s := range bad {
		if looksNumeric(s) {
			t.Errorf("looksNumeric(%q)=true, want false", s)
		}
	}
}
