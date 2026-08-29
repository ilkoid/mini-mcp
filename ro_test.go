package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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

func TestRejectUnsafeSQL_SemicolonInLiteralsAndComments(t *testing.T) {
	// A ';' inside data is not a statement separator.
	cases := []string{
		`SELECT 'a;b' AS s`,
		`SELECT $$a;b$$ AS s`,
		`SELECT $tag$a;b$tag$ AS s`,
		`SELECT 'don''t;' AS s`,
		`SELECT E'a\';b' AS s`,
		"SELECT 1 -- ; not a separator\n+ 2",
		`/* ; */ SELECT 1`,
		`SELECT string_to_array('a;b', ';')`,
		`SELECT 'semi;colon';`,
	}
	for _, in := range cases {
		if err := rejectUnsafeSQL(in); err != nil {
			t.Fatalf("expected OK for %q, got %v", in, err)
		}
	}
}

func TestRejectUnsafeSQL_RealSeparatorsStillRejected(t *testing.T) {
	cases := []string{
		`SELECT 'a'; DROP TABLE x`,
		`SELECT 'a' ;SELECT 1`,
		`SELECT $$x$$; SELECT 2`,
	}
	for _, in := range cases {
		if err := rejectUnsafeSQL(in); err == nil {
			t.Fatalf("expected rejection for %q", in)
		}
	}
}

func TestRejectUnsafeSQL_CommentLeadingKeyword(t *testing.T) {
	// Comments must not hide the real first keyword from the precheck.
	for _, in := range []string{
		"-- note\nDROP TABLE t",
		"/* hi */ DELETE FROM t",
	} {
		if err := rejectUnsafeSQL(in); err == nil {
			t.Fatalf("expected rejection for %q", in)
		}
	}
	if err := rejectUnsafeSQL("/* hi */ SELECT 1"); err != nil {
		t.Fatalf("expected OK, got %v", err)
	}
}

func TestRejectUnsafeSQL_SessionStatementsRejected(t *testing.T) {
	for _, in := range []string{
		"SET statement_timeout = 0",
		"SET LOCAL work_mem = '1GB'",
		"RESET ALL",
		"DISCARD ALL",
		"PREPARE p AS SELECT 1",
		"EXECUTE p",
		"LISTEN chan",
		"NOTIFY chan",
		"LOCK TABLE t",
		"CHECKPOINT",
		"BEGIN",
		"COMMIT",
	} {
		if err := rejectUnsafeSQL(in); err == nil {
			t.Fatalf("expected rejection for %q", in)
		}
	}
	// Read-only session introspection stays allowed.
	for _, in := range []string{"SHOW statement_timeout", "EXPLAIN SELECT 1"} {
		if err := rejectUnsafeSQL(in); err != nil {
			t.Fatalf("expected OK for %q, got %v", in, err)
		}
	}
}

func TestMaskLiteralsAndComments(t *testing.T) {
	// Everything inside literals/comments vanishes; real syntax survives.
	cases := map[string]string{
		`SELECT 'a;b'`:          `SELECT`,
		`SELECT $$a;b$$`:        `SELECT`,
		`SELECT $tag$a;$tag$ b`: `SELECT b`,
		`SELECT 'don''t;'`:      `SELECT`,
		`SELECT E'\';b'`:        `SELECT`,
		`SELECT xe';y'`:         `SELECT xe`, // glued E is an identifier, then a plain string
		"SELECT 1 -- ;x\n+2":    "SELECT 1 \n+2",
		`/* ; */ SELECT $1`:     `SELECT $1`,
		`SELECT $1, 'x'`:        `SELECT $1,`,
		`SELECT "c;d"`:          `SELECT`,
	}
	for in, want := range cases {
		got := maskLiteralsAndComments(in)
		if len(got) != len(in) {
			t.Fatalf("mask(%q) changed length: %q", in, got)
		}
		if g, w := strings.Join(strings.Fields(got), " "), strings.Join(strings.Fields(want), " "); g != w {
			t.Fatalf("mask(%q) fields = %q, want %q", in, g, w)
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

func TestEncodePgtypeNumeric(t *testing.T) {
	// pgx v5 hands numeric columns to rows.Values() as pgtype.Numeric VALUES —
	// the exact type that used to fall through to the "unexpected type" error.
	e := newRowEncoder([]uint32{oidNumeric})
	bigSum, ok := new(big.Int).SetString("9007199254740993", 10) // 2^53+1
	if !ok {
		t.Fatal("big.Int.SetString failed")
	}
	cases := []struct {
		in   any
		want string // expected JSON rendering
	}{
		{pgtype.Numeric{Int: big.NewInt(150), Exp: -2, Valid: true}, `"1.50"`},
		{pgtype.Numeric{Int: big.NewInt(-271828), Exp: -5, Valid: true}, `"-2.71828"`},
		{pgtype.Numeric{Int: bigSum, Valid: true}, `"9007199254740993"`},
		{pgtype.Numeric{Valid: false}, `null`},                    // NULL
		{pgtype.Numeric{NaN: true, Valid: true}, `"NaN"`},         // NaN
		{(*pgtype.Numeric)(nil), `null`},                          // nil pointer
		{&pgtype.Numeric{Int: big.NewInt(42), Valid: true}, `"42"`}, // pointer form
	}
	for _, c := range cases {
		row, _, err := e.encode([]any{c.in})
		if err != nil {
			t.Fatalf("encode(%#v): %v", c.in, err)
		}
		b, err := jsonMarshal(row[0])
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Fatalf("encode(%#v) = %s, want %s", c.in, b, c.want)
		}
	}
}

func TestResultFieldOrder_MetadataBeforeRows(t *testing.T) {
	// The ZCode client truncates oversized tool output by chopping the JSON
	// tail; row_count/truncated/notice must precede rows to survive that.
	b, err := jsonMarshal(&Result{
		Columns: []string{"c"}, RowCount: 1, Truncated: true, Notice: "n",
		Rows: [][]interface{}{{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	rowsAt := strings.Index(s, `"rows"`)
	if rowsAt < 0 {
		t.Fatalf("no rows field in %s", s)
	}
	for _, key := range []string{`"columns"`, `"row_count"`, `"truncated"`, `"notice"`} {
		if at := strings.Index(s, key); at < 0 || at > rowsAt {
			t.Fatalf("%s must precede \"rows\" in %s", key, s)
		}
	}
}

func TestTruncationNotice(t *testing.T) {
	big := truncationNotice(maxBytes + 1)
	if !strings.Contains(big, "single row") || !strings.Contains(big, "substring()") {
		t.Fatalf("single-row notice wrong: %q", big)
	}
	for _, chunk := range []int{0, 100, maxBytes} { // row-cap and mid-stream cases
		gen := truncationNotice(chunk)
		if !strings.Contains(gen, "maxRows") || strings.Contains(gen, "single row") {
			t.Fatalf("generic notice wrong for chunk=%d: %q", chunk, gen)
		}
	}
}

func TestEncodeChunkIncludesFraming(t *testing.T) {
	// The byte cap must count the row's real wire size: cells + '[', ']', ','.
	// Undercounting by ~3B/row let a 323-row result overshoot the client's
	// 50KB resultBudget by ~280B and get its tail chopped.
	e := newRowEncoder([]uint32{oidInt8})
	row, chunk, err := e.encode([]any{int64(12)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := jsonMarshal(row[0])
	if want := len(b) + 3; chunk != want {
		t.Fatalf("chunk=%d, want cell(%d)+framing 3 = %d", chunk, len(b), want)
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
