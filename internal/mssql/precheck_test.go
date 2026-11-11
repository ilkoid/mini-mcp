package mssql

import (
	"strings"
	"testing"

	"mini-mcp/internal/anon"
)

// Synthetic markers only — never real keywords.
const (
	kw    = "SampleWord"
	alias = "AliasWord"
)

func setAnonymizer(t *testing.T) {
	t.Helper()
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
}

func TestRejectUnsafeSQL_TSQLStructure(t *testing.T) {
	setAnonymizer(t)
	reject := func(sql string) {
		t.Helper()
		if err := RejectUnsafeSQL(sql); err == nil {
			t.Fatalf("expected rejection for %q", sql)
		}
	}
	accept := func(sql string) {
		t.Helper()
		if err := RejectUnsafeSQL(sql); err != nil {
			t.Fatalf("unexpected rejection for %q: %v", sql, err)
		}
	}

	reject("")
	reject("   ")
	reject("SELECT 1; SELECT 2")
	reject("SELECT 1;SELECT 2;")
	reject("SELECT 1; -- c\nDROP TABLE t")
	// Statement keywords.
	for _, kw := range []string{
		"INSERT INTO t VALUES (1)", "UPDATE t SET a=1", "DELETE FROM t",
		"TRUNCATE TABLE t", "CREATE TABLE t (id int)", "DROP TABLE t",
		"ALTER TABLE t ADD c int", "EXEC sp_help", "EXECUTE dbo.p",
		"sp_executesql N'SELECT 1'", "DECLARE @x int",
		"USE other_db", "SELECT 1\nGO\nSELECT 2",
		"SET NOCOUNT ON", "BEGIN TRAN", "PRINT 'x'",
		"BACKUP DATABASE m TO DISK = 'x'",
	} {
		reject(kw)
	}
	// Semicolons inside data, not separators.
	accept("SELECT * FROM t WHERE a = 'x;y'")
	accept("SELECT * FROM t WHERE a = N'x;y'")
	accept("SELECT [col;name] FROM [dbo].[my;table]")
	accept("SELECT * FROM t WHERE a = 'it''s;fine'")
	accept("SELECT 1 /* ; comment */ AS x")
	accept("-- leading comment\nSELECT 1")
	accept("SELECT TOP 10 * FROM dbo.cards ORDER BY 1")
	accept("WITH c AS (SELECT 1 AS n) SELECT * FROM c")
}

func TestRejectUnsafeSQL_BlockedLeakFunctions(t *testing.T) {
	setAnonymizer(t)
	for _, q := range []string{
		// ТЗ Д-1 substring family, T-SQL spellings.
		"SELECT LEFT(brand, 4) FROM cards",
		"SELECT RIGHT(brand, 4) FROM cards",
		"SELECT SUBSTRING(brand, 2, 3) FROM cards",
		"SELECT STUFF(brand, 1, 2, 'x') FROM cards",
		"SELECT REPLACE(brand, 'a', 'b') FROM cards",
		"SELECT TRIM(brand) FROM cards",
		"SELECT LTRIM(RTRIM(brand)) FROM cards",
		"SELECT REVERSE(brand) FROM cards",
		// Д-2 hashes/encodings.
		"SELECT HASHBYTES('MD5', brand) FROM cards",
		"SELECT HASHBYTES('SHA2_256', brand) FROM cards",
		"SELECT COMPRESS(brand) FROM cards",
		"SELECT DECOMPRESS(bin_col) FROM cards",
		// Д-3 lengths.
		"SELECT LEN(brand) FROM cards",
		"SELECT DATALENGTH(brand) FROM cards",
		// Boolean oracles.
		"SELECT CASE WHEN CHARINDEX('Fam', brand) > 0 THEN 1 ELSE 0 END FROM cards",
		"SELECT PATINDEX('%Fam%', brand) FROM cards",
		// VARBINARY cast pattern (Д-2).
		"SELECT CAST(brand AS VARBINARY(100)) FROM cards",
		"SELECT CAST(brand AS VARBINARY(max)) FROM cards",
		"SELECT CONVERT(VARBINARY(max), brand) FROM cards",
		"SELECT CONVERT(BINARY(8), brand) FROM cards",
		"SELECT CAST(brand AS binary) FROM cards",
		// Live leak regression (2026-09-04 test report, №20/№21/№22): the
		// exact Cyrillic-bracket shapes a tester session used — HASHBYTES and
		// LEFT executed and shipped real hashes/prefixes while ASCII probes
		// were blocked. The whole family must reject regardless of script.
		"SELECT TOP 5 [Марка], HASHBYTES('MD5', [Марка]) AS h FROM dbo.H_Sales_Rests_Collections_Stock_Size",
		"SELECT TOP 5 [Марка], LEFT([Марка], 4) AS p4 FROM dbo.H_Sales_Rests_Collections_Stock_Size",
		"SELECT TOP 5 [Марка], LEN([Марка]) AS l FROM dbo.H_Sales_Rests_Collections_Stock_Size",
		"SELECT TOP 5 [Марка], RIGHT([Марка], 4) AS p4 FROM dbo.t",
		"SELECT HASHBYTES('SHA2_256', [Марка]) FROM dbo.t",
	} {
		err := RejectUnsafeSQL(q)
		if err == nil {
			t.Fatalf("expected guard rejection for %q", q)
		}
		if !strings.Contains(err.Error(), "anonymization guard") {
			t.Fatalf("wrong error for %q: %v", q, err)
		}
	}
}

func TestRejectUnsafeSQL_BlockedNoFalsePositives(t *testing.T) {
	setAnonymizer(t)
	for _, q := range []string{
		// Whole-value transforms, aggregates, windows, casts to text — all fine.
		"SELECT UPPER(brand) FROM cards",
		"SELECT LOWER(brand) FROM cards",
		"SELECT CONCAT(a, N' ', b) FROM cards",
		"SELECT STRING_AGG(brand, ',') FROM cards",
		"SELECT COUNT(*), SUM(qty) FROM sales",
		"SELECT ROUND(AVG(price), 2) FROM sales",
		"SELECT CAST(price AS VARCHAR(40)) FROM sales",
		"SELECT CONVERT(NVARCHAR(100), name) FROM cards",
		"SELECT ROW_NUMBER() OVER (ORDER BY id) FROM cards",
		"SELECT COALESCE(notes, N'') FROM cards",
		"SELECT DATEADD(day, 1, created_at) FROM cards",
		"SELECT brand_md5_hint FROM t",        // identifier containing a name, no call
		"SELECT t.left FROM t",                // qualified column named left
		"SELECT 'use LEFT carefully' AS note", // literal text mentioning a name
		"SELECT * FROM t WHERE note = 'LEN(brand)'",
	} {
		if err := RejectUnsafeSQL(q); err != nil {
			t.Fatalf("unexpected rejection for %q: %v", q, err)
		}
	}
}

func TestRejectUnsafeSQL_BlockedOverride(t *testing.T) {
	a, err := anon.NewWithConfig(&anon.Config{
		Rules:    []anon.Rule{{ReadSubstring: kw, WriteSubstring: alias}},
		Security: &anon.Security{BlockedFunctionsOverride: []string{"hashbytes"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	Anonymizer = a
	t.Cleanup(func() { Anonymizer = nil })

	if err := RejectUnsafeSQL("SELECT HASHBYTES('MD5', brand) FROM t"); err == nil {
		t.Fatal("override list must still block hashbytes")
	}
	if err := RejectUnsafeSQL("SELECT LEN(brand) FROM t"); err != nil {
		t.Fatalf("override list must drop len from the blocklist: %v", err)
	}
}

func TestUnmapThenExpand_TSQLFilter(t *testing.T) {
	setAnonymizer(t)
	// The exact composition RunReadOnly applies: Unmap restores the keyword,
	// then the full-literal filter expands to every stored spelling —
	// including the N'…' national-string form T-SQL agents write.
	want := "IN (N'SampleWord',N'sampleword',N'AliasWord')"
	for _, in := range []string{
		"SELECT * FROM cards WHERE brand = 'AliasWord'",
		"SELECT * FROM cards WHERE brand = N'AliasWord'",
		"SELECT * FROM cards WHERE brand=N'AliasWord'",
	} {
		sql := Anonymizer.Unmap(in)
		sql = Anonymizer.ExpandAliasComparisons(sql, renderTSQLComparison)
		if !strings.Contains(sql, want) {
			t.Fatalf("filter not expanded for %q:\n got %s\nwant substring %s", in, sql, want)
		}
		// The render must never glue IN onto the operand ("brandIN").
		if strings.Contains(sql, "brandIN") || strings.Contains(sql, "brand NOT IN") {
			t.Fatalf("IN glued to operand for %q: %s", in, sql)
		}
	}
	// LIKE full literal maps to IN too (semantically equal, no LIKE ANY in T-SQL).
	sql := Anonymizer.Unmap("SELECT * FROM cards WHERE brand LIKE 'AliasWord'")
	sql = Anonymizer.ExpandAliasComparisons(sql, renderTSQLComparison)
	if !strings.Contains(sql, want) {
		t.Fatalf("LIKE full-literal not expanded: %s", sql)
	}
	// Negated form.
	sql = Anonymizer.Unmap("SELECT * FROM cards WHERE brand <> N'AliasWord'")
	sql = Anonymizer.ExpandAliasComparisons(sql, renderTSQLComparison)
	if !strings.Contains(sql, "NOT IN (N'SampleWord',N'sampleword',N'AliasWord')") {
		t.Fatalf("negated filter not expanded: %s", sql)
	}
	// Wildcard patterns and unrelated literals stay untouched.
	for _, in := range []string{
		"SELECT * FROM cards WHERE brand LIKE N'%Alias%'",
		"SELECT * FROM cards WHERE brand = N'SomeoneElse'",
		"SELECT * FROM cards WHERE price > 100 AND brand <> N'X'",
	} {
		sql := Anonymizer.ExpandAliasComparisons(Anonymizer.Unmap(in), renderTSQLComparison)
		if strings.Contains(sql, "IN (N'") {
			t.Fatalf("unexpected expansion for %q: %s", in, sql)
		}
	}
}

func TestRenderTSQLComparison(t *testing.T) {
	if got := renderTSQLComparison("=", false, []string{"A", "B"}); got != " IN (N'A',N'B')" {
		t.Fatalf("plain: %s", got)
	}
	if got := renderTSQLComparison("=", true, []string{"A"}); got != " NOT IN (N'A')" {
		t.Fatalf("negated: %s", got)
	}
	if got := renderTSQLComparison("LIKE", false, []string{"A'B"}); got != ` IN (N'A''B')` {
		t.Fatalf("quote escaping: %s", got)
	}
}

// TestMaskLiteralsAndComments_BracketShape pins the live-leak regression
// (2026-09-04): a [bracket identifier] must mask ONLY up to its closing ']'
// — the scanner used to hunt for the next '[', swallowing `, HASHBYTES(`
// into the "literal" and hiding it from the blocked-function guard.
func TestMaskLiteralsAndComments_BracketShape(t *testing.T) {
	q := "SELECT TOP 5 [Марка], HASHBYTES('MD5', [Марка]) AS h FROM dbo.t"
	masked := maskLiteralsAndComments(q)
	if strings.Contains(strings.ToUpper(masked), "МАРКА") {
		t.Fatalf("bracket content leaked into masked copy: %q", masked)
	}
	if !strings.Contains(strings.ToUpper(masked), "HASHBYTES(") {
		t.Fatalf("function name swallowed by bracket masking: %q", masked)
	}
	if !strings.Contains(masked, "SELECT TOP 5") {
		t.Fatalf("leading SQL mangled: %q", masked)
	}

	// ';' between two bracket identifiers is DATA, not a separator...
	if strings.ContainsRune(maskLiteralsAndComments("SELECT [a;b] AS x"), ';') {
		t.Fatal("';' inside brackets must be masked as data")
	}
	// ...but a real ';' after a closed bracket is a statement separator.
	if !strings.ContainsRune(maskLiteralsAndComments("SELECT [a]; SELECT 2"), ';') {
		t.Fatal("';' after a closed bracket must stay visible (multi-statement)")
	}
}
