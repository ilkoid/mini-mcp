package anon

import (
	"errors"
	"strings"
	"testing"
)

// Synthetic markers only — never real keywords. Guards verified here are the
// layers added for the independent test report 2026-09-03 (Д-1…Д-7).

func TestApply_UpperTokenGetsUpperAlias(t *testing.T) {
	// Д-6: upper(brand) is computed in the DB before masking, so an all-upper
	// token must yield the all-upper alias; everything else stays canonical
	// (lower is NOT mirrored — that keeps the existing masking contract).
	a := mustNew(t, []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}})
	cases := map[string]string{
		"SAMPLEWORD":     "ALIASWORD",
		"SAMPLEWORD123":  "ALIASWORD123", // digits don't break upper detection
		"ооо SAMPLEWORD": "ооо ALIASWORD",
		"sampleword":     "AliasWord", // not lower-mirrored: canonical
		"SampleWord":     "AliasWord",
		"123":            "123", // no letters: untouched by any rule anyway
	}
	for in, want := range cases {
		if got := a.Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApply_UpperTokenCyrillicRule(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "ООО Ромашка", WriteSubstring: "Клиент-7"}})
	if got := a.Apply("ООО РОМАШКА"); got != "КЛИЕНТ-7" {
		t.Fatalf("upper Cyrillic token must get upper alias: %q", got)
	}
	if got := a.Apply("контрагент ооо ромашка, г.М"); got != "контрагент Клиент-7, г.М" {
		t.Fatalf("lower/mixed must stay canonical: %q", got)
	}
}

func TestUnmap_CaseNotPreserved(t *testing.T) {
	// The restored keyword keeps the yaml spelling verbatim — that is what
	// makes '=' filters predictable and lets eq_any spellings match exactly.
	a := mustNew(t, []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}})
	if got := a.Unmap("ALIASWORD"); got != "SampleWord" {
		t.Fatalf("Unmap = %q, want canonical %q", got, "SampleWord")
	}
}

func TestGuardFragments(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}})
	// Д-1: a 4-char fragment of the keyword that survived Apply is redacted.
	got, hit := a.GuardFragments("x Samp y")
	if !hit || got != "x … y" {
		t.Fatalf("fragment not redacted: %q hit=%v", got, hit)
	}
	// Clean text passes through untouched.
	got, hit = a.GuardFragments("clean text stays")
	if hit || got != "clean text stays" {
		t.Fatalf("clean text touched: %q hit=%v", got, hit)
	}
	// "Word" occurs in the keyword AND inside the alias — the alias is
	// shielded (it is public), the standalone fragment is not.
	got, hit = a.GuardFragments("AliasWord Word")
	if !hit || got != "AliasWord …" {
		t.Fatalf("alias shield failed: %q hit=%v", got, hit)
	}
	// Whole keyword is caught too (callers normally Apply first).
	got, hit = a.GuardFragments("SampleWord")
	if !hit || got != "…" {
		t.Fatalf("full keyword must redact: %q hit=%v", got, hit)
	}
}

func TestGuardFragments_MinLenAndDisable(t *testing.T) {
	base := []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}}
	a, err := NewWithConfig(&Config{Rules: base, Security: &Security{FragmentMinLen: 6}})
	if err != nil {
		t.Fatal(err)
	}
	if got, hit := a.GuardFragments("x Samp y"); hit || got != "x Samp y" {
		t.Fatalf("4-char fragment must survive under min=6: %q hit=%v", got, hit)
	}
	if _, hit := a.GuardFragments("x Sample y"); !hit { // "Sample" is 6 chars
		t.Fatal("6-char fragment must be redacted under min=6")
	}
	off, err := NewWithConfig(&Config{Rules: base, Security: &Security{FragmentMinLen: -1}})
	if err != nil {
		t.Fatal(err)
	}
	if got, hit := off.GuardFragments("SampleWord"); hit || got != "SampleWord" {
		t.Fatalf("negative min must disable the guard: %q hit=%v", got, hit)
	}
}

func TestGuardFragments_NilAnonymizer(t *testing.T) {
	var a *Anonymizer
	if s, hit := a.GuardFragments("Samp"); hit || s != "Samp" {
		t.Fatalf("nil must pass through: %q hit=%v", s, hit)
	}
}

func TestCheckBlockedFunctions(t *testing.T) {
	blocked := []string{"md5", "left", "length"}
	hits := []string{
		"SELECT md5(brand) FROM t",
		"SELECT MD5 (brand) FROM t", // space before paren is legal SQL
		"SELECT left(brand, 4) FROM t",
		"select length(x) from t",
		"SELECT md5\n(brand) FROM t",
	}
	for _, sql := range hits {
		if _, ok := CheckBlockedFunctions(sql, blocked); !ok {
			t.Errorf("expected block for %q", sql)
		}
	}
	misses := []string{
		"SELECT brand FROM t",                  // no call at all
		"SELECT brand_md5 FROM t",              // glued identifier, not a call
		"SELECT t.left FROM t",                 // column ref without paren
		"SELECT 'uses md5 internally' AS note", // name in data, no call
	}
	for _, sql := range misses {
		if name, ok := CheckBlockedFunctions(sql, blocked); ok {
			t.Errorf("unexpected block %q for %q", name, sql)
		}
	}
}

func TestBlockedFunctionsOverride(t *testing.T) {
	a, err := NewWithConfig(&Config{
		Rules:    []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}},
		Security: &Security{BlockedFunctionsOverride: []string{"md5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := a.BlockedFunctions([]string{"left", "length"})
	if len(got) != 1 || got[0] != "md5" {
		t.Fatalf("override must replace defaults wholesale: %v", got)
	}
	var nilA *Anonymizer
	if got := nilA.BlockedFunctions([]string{"left"}); len(got) != 1 || got[0] != "left" {
		t.Fatal("nil anonymizer must fall back to dialect defaults")
	}
}

func renderForTest(op string, negated bool, variants []string) string {
	q := make([]string, len(variants))
	for i, v := range variants {
		q[i] = "'" + v + "'"
	}
	prefix := op
	if negated {
		prefix = "NOT " + op
	}
	return prefix + " (" + strings.Join(q, ",") + ")"
}

func TestExpandAliasComparisons_TwoEra(t *testing.T) {
	// Д-4/Д-5: eq_any lists every stored spelling — the yaml keyword, the
	// lowercase variant stored in old-era rows, and the literal alias written
	// into new-era rows by the upstream anonymizer.
	a, err := NewWithConfig(&Config{Rules: []Rule{{
		ReadSubstring:  "SampleWord",
		WriteSubstring: "AliasWord",
		EqAny:          []string{"sampleword", "AliasWord"},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	in := "SELECT * FROM t WHERE brand = 'SampleWord'"
	want := "SELECT * FROM t WHERE brand = ('SampleWord','sampleword','AliasWord')"
	if got := a.ExpandAliasComparisons(in, renderForTest); got != want {
		t.Fatalf("= expansion: %q", got)
	}

	// Case variants of the literal match too; LIKE/ILIKE/negated forms expand.
	in = "WHERE b ILIKE 'aliasword' AND c NOT LIKE 'SampleWord' AND d != 'SampleWord'"
	got := a.ExpandAliasComparisons(in, renderForTest)
	for _, want := range []string{
		"ILIKE ('SampleWord','sampleword','AliasWord')",
		"NOT LIKE ('SampleWord','sampleword','AliasWord')",
		"NOT = ('SampleWord','sampleword','AliasWord')",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}

	// Wildcard patterns are NOT translated (Д-7); unrelated literals untouched.
	in = "WHERE b LIKE '%Sample%' AND c = 'Disney'"
	if got := a.ExpandAliasComparisons(in, renderForTest); got != in {
		t.Fatalf("wildcards/unrelated must stay untouched: %q", got)
	}
	// E'' escape literals are never touched.
	in = `WHERE b = E'SampleWord'`
	if got := a.ExpandAliasComparisons(in, renderForTest); got != in {
		t.Fatalf("E-literal must stay untouched: %q", got)
	}
	// A literal that only equals an eq_any variant also expands (the agent
	// may have copied a stored spelling from an earlier result).
	in = "WHERE b = 'sampleword'"
	if got := a.ExpandAliasComparisons(in, renderForTest); !strings.Contains(got, "('SampleWord','sampleword','AliasWord')") {
		t.Fatalf("variant literal must expand: %q", got)
	}
	// Doubled-quote escaping inside literals survives the scan.
	in = `WHERE b = 'don''t SampleWord'`
	if got := a.ExpandAliasComparisons(in, renderForTest); got != in {
		t.Fatalf("non-matching literal must stay untouched: %q", got)
	}
}

func TestExpandAliasComparisons_NoEqAnyNoRewrite(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}})
	in := "WHERE b = 'SampleWord'"
	if got := a.ExpandAliasComparisons(in, renderForTest); got != in {
		t.Fatalf("without eq_any nothing must be rewritten: %q", got)
	}
}

func TestHasAliasEquality(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}})
	if !a.HasAliasEquality("SELECT * FROM t WHERE b = 'AliasWord'", nil) {
		t.Fatal("= alias literal must be detected")
	}
	if !a.HasAliasEquality("SELECT * FROM t WHERE b = 'ALIASWORD'", nil) {
		t.Fatal("CI alias literal must be detected")
	}
	if !a.HasAliasEquality("SELECT * FROM t WHERE b='AliasWord'", nil) {
		t.Fatal("no-space = must be detected")
	}
	if a.HasAliasEquality("SELECT * FROM t WHERE b ILIKE 'AliasWord'", nil) {
		t.Fatal("ILIKE is not equality — no detection")
	}
	if a.HasAliasEquality("SELECT * FROM t WHERE b = 'Disney'", nil) {
		t.Fatal("unrelated literal must not be detected")
	}
	if !a.HasAliasEquality("SELECT * FROM t WHERE b = $1", []any{"AliasWord"}) {
		t.Fatal("bind arg equal to the alias must be detected")
	}
	if a.HasAliasEquality("SELECT * FROM t WHERE b = $1", []any{"Disney"}) {
		t.Fatal("unrelated bind must not be detected")
	}
	var nilA *Anonymizer
	if nilA.HasAliasEquality("SELECT 'AliasWord'", nil) {
		t.Fatal("nil anonymizer must not detect")
	}
}

func TestHasPartialPattern(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}})
	if !a.HasPartialPattern("WHERE b ILIKE '%liaswor%'") {
		t.Fatal("partial alias fragment must warn (Д-7)")
	}
	if a.HasPartialPattern("WHERE b ILIKE '%AliasWord %'") {
		t.Fatal("pattern containing the full alias IS translated — no warn")
	}
	if a.HasPartialPattern("WHERE b ILIKE 'AliasWord'") {
		t.Fatal("full-value pattern — no warn")
	}
	if a.HasPartialPattern("WHERE b ILIKE '%Disney%'") {
		t.Fatal("unrelated pattern — no warn")
	}
	if a.HasPartialPattern("WHERE b = 'AliasWord'") {
		t.Fatal("equality is not a pattern")
	}
}

func TestApplyErr_GuardsFragments(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}})
	got := a.ApplyErr(errors.New("value Samp rejected"))
	if got.Error() != "value … rejected" {
		t.Fatalf("ApplyErr must fragment-guard: %q", got.Error())
	}
}

func TestApplyJSON_GuardsFragments(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "SampleWord", WriteSubstring: "AliasWord"}})
	got := string(a.ApplyJSON([]byte(`{"frag":"Samp"}`)))
	if strings.Contains(got, "Samp") {
		t.Fatalf("json fragment leaked: %s", got)
	}
}

func TestLoad_SecurityTunables(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/rules.yaml"
	if err := writeFile(p, "rules:\n  - read_substring: SampleWord\n    write_substring: AliasWord\nsecurity:\n  fragment_min_len: 5\n"); err != nil {
		t.Fatal(err)
	}
	a, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, hit := a.GuardFragments("x Samp y"); hit {
		t.Fatal("fragment_min_len: 5 must not redact 4-char fragments")
	}
}

func TestExpandAliasComparisons_NPrefix(t *testing.T) {
	// T-SQL national strings: = N'x' / LIKE N'x' — the N is glued to the
	// literal and must not hide the operator from the scanner (MSSQL stage
	// B; the rewrite absorbs the N into the replaced span).
	a := mustNew(t, []Rule{{
		ReadSubstring:  "SampleWord",
		WriteSubstring: "AliasWord",
		EqAny:          []string{"sampleword"},
	}})
	pg := func(op string, negated bool, variants []string) string {
		quoted := make([]string, len(variants))
		for i, v := range variants {
			quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		arr := "ARRAY[" + strings.Join(quoted, ",") + "]"
		if op == "=" && !negated {
			return "= ANY(" + arr + ")"
		}
		return op + " ANY (" + arr + ")"
	}
	// Expansion judges the POST-Unmap SQL (keyword spellings), exactly like
	// RunReadOnly composes it.
	sql := a.ExpandAliasComparisons(a.Unmap("WHERE brand = N'AliasWord'"), pg)
	if !strings.Contains(sql, "= ANY(ARRAY['SampleWord','sampleword'])") {
		t.Fatalf("N-prefixed equality not expanded: %s", sql)
	}
	sql = a.ExpandAliasComparisons(a.Unmap("WHERE brand LIKE N'AliasWord'"), pg)
	if !strings.Contains(sql, "LIKE ANY (ARRAY['SampleWord','sampleword'])") {
		t.Fatalf("N-prefixed LIKE not expanded: %s", sql)
	}
	// The N glued to an identifier (invalid SQL) must not count as a prefix.
	sql = a.ExpandAliasComparisons("WHERE brandxN'AliasWord'", pg)
	if strings.Contains(sql, "ANY") {
		t.Fatalf("identifier-glued N misdetected as prefix: %s", sql)
	}
	// Equality and partial-pattern detectors see through the N too.
	if !a.HasAliasEquality("WHERE brand = N'AliasWord'", nil) {
		t.Fatal("HasAliasEquality missed N-prefixed literal")
	}
	if !a.HasPartialPattern("WHERE brand LIKE N'%liaswor%'") {
		t.Fatal("HasPartialPattern missed N-prefixed pattern")
	}
}
