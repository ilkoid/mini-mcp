package pg

import (
	"encoding/json"
	"strings"
	"testing"

	"mini-mcp/internal/anon"
)

// Synthetic markers only — never real keywords.

// setGuardAnonymizer installs a synthetic two-era masker: the yaml keyword,
// a lowercase stored variant, and the literal alias in new-era rows.
func setGuardAnonymizer(t *testing.T) {
	t.Helper()
	a, err := anon.NewWithConfig(&anon.Config{
		Rules: []anon.Rule{
			{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord", EqAny: []string{"sampleword", "AliasWord"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	Anonymizer = a
	t.Cleanup(func() { Anonymizer = nil })
}

// The Д-1/Д-2/Д-3 acceptance queries from the independent test report must be
// rejected by the CLIENT precheck with the guard error (nothing is sent).
func TestRejectUnsafeSQL_BlockedLeakFunctions(t *testing.T) {
	for _, sql := range []string{
		"SELECT left(brand, 4), count(*) FROM cards GROUP BY 1",
		"SELECT right(brand, 4), count(*) FROM cards GROUP BY 1",
		"SELECT substring(brand from 1 for 4), count(*) FROM cards GROUP BY 1",
		"SELECT split_part(brand, ' ', 1), count(*) FROM cards GROUP BY 1",
		"SELECT btrim(brand, 'Fam'), count(*) FROM cards GROUP BY 1",
		"SELECT reverse(brand), count(*) FROM cards GROUP BY 1",
		"SELECT initcap(brand), count(*) FROM cards GROUP BY 1",
		"SELECT strpos(brand, 'Fam') > 0, count(*) FROM cards GROUP BY 1",
		"SELECT md5(brand) FROM cards GROUP BY 1",
		"SELECT encode(brand::bytea, 'base64') FROM cards GROUP BY 1",
		"SELECT brand, length(brand) FROM cards GROUP BY 1, 2",
		"SELECT convert_to(brand, 'UTF8') FROM cards",
	} {
		err := RejectUnsafeSQL(sql)
		if err == nil || !strings.Contains(err.Error(), "anonymization guard") {
			t.Errorf("expected guard rejection for %q, got %v", sql, err)
		}
	}
}

// The regression minimum Р-1…Р-11 from the report must keep working.
func TestRejectUnsafeSQL_BlockedFunctionsNoFalsePositives(t *testing.T) {
	for _, sql := range []string{
		"SELECT brand, count(*) FROM cards GROUP BY brand",
		"SELECT upper(brand), count(*) FROM cards GROUP BY 1",
		"SELECT '##' || brand || '##'",
		"SELECT string_agg(DISTINCT brand, ' | ') FROM cards",
		"SELECT brand_md5_note FROM t", // identifier, not a call
		"SELECT search_text FROM t WHERE search_text ILIKE '%AliasWord %'",
		"SELECT round(1.23456::numeric, 2)",
		"SELECT i FROM generate_series(1, 600) AS t(i)",
	} {
		if err := RejectUnsafeSQL(sql); err != nil {
			t.Errorf("expected OK for %q, got %v", sql, err)
		}
	}
}

func TestEncodeCell_GuardFragmentsRedactsDerivatives(t *testing.T) {
	setGuardAnonymizer(t)
	e := newRowEncoder([]uint32{oidText})
	row, _, err := e.encode([]any{"cut Samp rest"})
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != "cut … rest" {
		t.Fatalf("fragment survived: %v", row[0])
	}
	if e.guardHits != 1 {
		t.Fatalf("guardHits = %d, want 1", e.guardHits)
	}
	// Clean strings don't count as guarded.
	e2 := newRowEncoder([]uint32{oidText})
	if _, _, err := e2.encode([]any{"ordinary text"}); err != nil {
		t.Fatal(err)
	}
	if e2.guardHits != 0 {
		t.Fatal("clean cell must not count as guarded")
	}
}

func TestEncodeBytea_TextualGuard(t *testing.T) {
	setGuardAnonymizer(t)
	e := newRowEncoder([]uint32{oidBytea})

	// Keyword as utf-8 bytea → masked TEXT, never base64 of the original.
	row, _, err := e.encode([]any{[]byte("pre SampleWord post")})
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != "pre AliasWord post" {
		t.Fatalf("keyword bytea not masked: %v", row[0])
	}

	// Fragment-only utf-8 bytea → redacted + counted.
	row, _, err = e.encode([]any{[]byte("value Samp")})
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != "value …" {
		t.Fatalf("fragment bytea not guarded: %v", row[0])
	}
	if e.guardHits != 1 {
		t.Fatalf("guardHits = %d, want 1", e.guardHits)
	}

	// UTF-16LE with BOM (what convert_to(text,'UTF16') makes) → masked text.
	enc := []byte{0xFF, 0xFE}
	for _, r := range "SampleWord" {
		enc = append(enc, byte(r), 0)
	}
	row, _, err = e.encode([]any{enc})
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != "AliasWord" {
		t.Fatalf("utf16 keyword not masked: %v", row[0])
	}

	// Real binary keeps the base64 path (json.RawMessage of the base64 text).
	row, _, err = e.encode([]any{[]byte{0x00, 0x01, 0xFF}})
	if err != nil {
		t.Fatal(err)
	}
	rm, ok := row[0].(json.RawMessage)
	if !ok {
		t.Fatalf("binary bytea must stay a RawMessage, got %T", row[0])
	}
	if string(rm) != `"AAH/"` {
		t.Fatalf("binary bytea base64 wrong: %s", rm)
	}
}

func TestRenderPGComparison(t *testing.T) {
	if got := renderPGComparison("=", false, []string{"a", "b'c"}); got != `= ANY(ARRAY['a','b''c'])` {
		t.Fatalf("quote escaping broken: %s", got)
	}
	if got := renderPGComparison("=", true, []string{"a"}); got != `<> ALL(ARRAY['a'])` {
		t.Fatalf("negated equality semantics broken: %s", got)
	}
	if got := renderPGComparison("ILIKE", false, []string{"a"}); got != `ILIKE ANY (ARRAY['a'])` {
		t.Fatalf("ILIKE render: %s", got)
	}
	if got := renderPGComparison("LIKE", true, []string{"a"}); got != `NOT LIKE ANY (ARRAY['a'])` {
		t.Fatalf("NOT LIKE render: %s", got)
	}
}

// The exact composition RunReadOnly performs: Unmap (alias→keyword) then
// eq_any expansion — an agent's alias equality filter must reach BOTH data
// eras (pre-migration keyword rows AND post-migration literal-alias rows).
func TestUnmapThenExpand_TwoEraFilter(t *testing.T) {
	setGuardAnonymizer(t)
	agentSQL := "SELECT count(*) FROM cards WHERE brand = 'AliasWord'"
	expanded := Anonymizer.ExpandAliasComparisons(Anonymizer.Unmap(agentSQL), renderPGComparison)
	want := "SELECT count(*) FROM cards WHERE brand = ANY(ARRAY['SampleWord','sampleword','AliasWord'])"
	if expanded != want {
		t.Fatalf("two-era expansion: %q", expanded)
	}
}

// Guard notices fire on the AGENT's original SQL (pre-Unmap), not on the
// restored text.
func TestNoticeDetectorsOnOriginalSQL(t *testing.T) {
	setGuardAnonymizer(t)
	// Д-5: = by alias (would silently return 0 rows on case mismatch).
	if !Anonymizer.HasAliasEquality("SELECT * FROM t WHERE b = 'AliasWord'", nil) {
		t.Fatal("= by alias on original SQL must be detected")
	}
	// Д-7: partial alias fragment in a pattern.
	if !Anonymizer.HasPartialPattern("SELECT * FROM t WHERE b LIKE '%liaswor%'") {
		t.Fatal("partial alias pattern must be detected")
	}
	// Full alias inside a pattern is translated — no warning.
	if Anonymizer.HasPartialPattern("SELECT * FROM t WHERE b ILIKE '%AliasWord %'") {
		t.Fatal("full alias pattern must not warn")
	}
}
