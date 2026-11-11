package anon

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func mustNew(t *testing.T, rules []Rule) *Anonymizer {
	t.Helper()
	a, err := New(rules)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestApply_SubstringCaseInsensitive(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "ООО Ромашка", WriteSubstring: "Клиент-7"}})
	cases := map[string]string{
		"ООО Ромашка": "Клиент-7",
		"ооо ромашка": "Клиент-7",
		// Д-6 (test report 2026-09-03): an ALL-UPPERCASE token (upper(col)
		// computed in the DB) renders the UPPERCASE alias — so masked rows
		// and literal-alias rows look identical under upper().
		"контрагент ООО РОМАШКА, г.М": "контрагент КЛИЕНТ-7, г.М",
		"Иванов": "Иванов", // untouched
		"":       "",
	}
	for in, want := range cases {
		if got := a.Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApply_LongestFirst(t *testing.T) {
	a := mustNew(t, []Rule{
		{ReadSubstring: "Ромашка", WriteSubstring: "X"},
		{ReadSubstring: "ООО Ромашка", WriteSubstring: "Клиент-7"},
	})
	if got := a.Apply("ООО Ромашка"); got != "Клиент-7" {
		t.Fatalf("longest keyword must win: got %q", got)
	}
	if got := a.Apply("просто Ромашка"); got != "просто X" {
		t.Fatalf("shorter keyword still applies elsewhere: got %q", got)
	}
}

func TestApply_ExactMode(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "ООО Ромашка", WriteSubstring: "Клиент-7", Exact: true}})
	if got := a.Apply("ООО Ромашка"); got != "Клиент-7" {
		t.Fatalf("exact whole-value match failed: %q", got)
	}
	if got := a.Apply("контрагент ООО Ромашка"); got != "контрагент ООО Ромашка" {
		t.Fatalf("exact rule must not fire inside longer text: %q", got)
	}
	if got := a.Apply("ооо ромашка"); got != "Клиент-7" {
		t.Fatalf("exact default stays case-insensitive: %q", got)
	}
}

func TestApply_CaseSensitive(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "Ромашка", WriteSubstring: "X", CaseSensitive: true}})
	if got := a.Apply("ООО Ромашка"); got != "ООО X" {
		t.Fatalf("CS match failed: %q", got)
	}
	if got := a.Apply("ооо ромашка"); got != "ооо ромашка" {
		t.Fatalf("CS rule must not match other case: %q", got)
	}
}

func TestUnmap_RestoresOriginal(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "ООО Ромашка", WriteSubstring: "Клиент-7"}})
	sql := "SELECT sum(amount) FROM orders WHERE client_name = 'Клиент-7'"
	want := "SELECT sum(amount) FROM orders WHERE client_name = 'ООО Ромашка'"
	if got := a.Unmap(sql); got != want {
		t.Fatalf("Unmap = %q, want %q", got, want)
	}
}

func TestUnmap_LongestFirstAndRoundtrip(t *testing.T) {
	a := mustNew(t, []Rule{
		{ReadSubstring: "ООО Ромашка", WriteSubstring: "Клиент-7"},
		{ReadSubstring: "Ромашка", WriteSubstring: "Клиент"},
	})
	if got := a.Unmap("Клиент-7 и Клиент"); got != "ООО Ромашка и Ромашка" {
		t.Fatalf("Unmap longest-first failed: %q", got)
	}
	masked := a.Apply(a.Unmap("WHERE x = 'Клиент-7'"))
	want := "WHERE x = 'Клиент-7'"
	if masked != want {
		t.Fatalf("Apply(Unmap(s)) must be identity on aliases: %q", masked)
	}
}

func TestEmptyWriteSubstringRejected(t *testing.T) {
	// Contract (matches the reference anonymizer): write_substring is
	// inserted verbatim and must not be empty — an empty value is a config
	// error, not redaction.
	if _, err := New([]Rule{{ReadSubstring: "секрет", WriteSubstring: ""}}); err == nil ||
		!strings.Contains(err.Error(), "empty write_substring") {
		t.Fatalf("empty write_substring must be rejected: %v", err)
	}
}

func TestNumericRules(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "7701234567", WriteSubstring: "ИНН-1"}})
	if alias, ok := a.ApplyNumericString("7701234567"); !ok || alias != "ИНН-1" {
		t.Fatalf("numeric exact match failed: %q %v", alias, ok)
	}
	if _, ok := a.ApplyNumericString("7701234568"); ok {
		t.Fatal("numeric mismatch must not hit")
	}
	if !a.HasNumericRules() {
		t.Fatal("HasNumericRules must be true")
	}
	// Numeric keywords also mask as plain strings.
	if got := a.Apply("inn=7701234567"); got != "inn=ИНН-1" {
		t.Fatalf("numeric keyword as substring: %q", got)
	}
}

func TestApplyJSON(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "ООО Ромашка", WriteSubstring: "Клиент-7"}})
	in := []byte(`{"client":"ООО Ромашка","note":"для ООО Ромашка","items":[{"name":"ООО Ромашка"},{"name":"ok"}],"n":42}`)
	got := string(a.ApplyJSON(in))
	for _, want := range []string{`"client":"Клиент-7"`, `"note":"для Клиент-7"`, `"name":"Клиент-7"`} {
		if !strings.Contains(got, want) {
			t.Errorf("ApplyJSON missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "Ромашка") {
		t.Errorf("ApplyJSON leaked a keyword: %s", got)
	}
	if !strings.Contains(got, `"n":42`) {
		t.Errorf("ApplyJSON must not touch unrelated numbers: %s", got)
	}
}

func TestApplyJSON_NumericValue(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "7701234567", WriteSubstring: "ИНН-1"}})
	got := string(a.ApplyJSON([]byte(`{"inn":7701234567}`)))
	if got != `{"inn":"ИНН-1"}` {
		t.Fatalf("numeric JSON masking failed: %s", got)
	}
}

func TestApplyJSON_KeysAndInvalid(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "Ромашка", WriteSubstring: "X"}})
	got := string(a.ApplyJSON([]byte(`{"Ромашка поле":1}`)))
	if got != `{"X поле":1}` {
		t.Fatalf("key masking failed: %s", got)
	}
	if got := string(a.ApplyJSON([]byte{0xff, 0xfe})); got != "\xff\xfe" {
		t.Fatalf("invalid JSON must pass through unchanged")
	}
}

func TestApplyErr(t *testing.T) {
	a := mustNew(t, []Rule{{ReadSubstring: "ООО Ромашка", WriteSubstring: "Клиент-7"}})
	err := errors.New(`duplicate key value "ООО Ромашка" violates constraint`)
	got := a.ApplyErr(err)
	if got.Error() != `duplicate key value "Клиент-7" violates constraint` {
		t.Fatalf("ApplyErr: %q", got)
	}
	if a.ApplyErr(nil) != nil {
		t.Fatal("nil error must stay nil")
	}
}

func TestNilAnonymizerPassthrough(t *testing.T) {
	var a *Anonymizer
	if a.Apply("x") != "x" || a.Unmap("x") != "x" {
		t.Fatal("nil Apply/Unmap must pass through")
	}
	if a.Len() != 0 || a.HasNumericRules() {
		t.Fatal("nil Len/HasNumericRules")
	}
	if _, ok := a.ApplyNumericString("1"); ok {
		t.Fatal("nil ApplyNumericString must miss")
	}
	if got := a.ApplyErr(errors.New("e")); got.Error() != "e" {
		t.Fatal("nil ApplyErr must pass through")
	}
	if got := a.ApplyJSON([]byte(`{"a":1}`)); string(got) != `{"a":1}` {
		t.Fatal("nil ApplyJSON must pass through")
	}
}

func TestValidation_ErrorsWithoutLeakingValues(t *testing.T) {
	cases := []struct {
		name  string
		rules []Rule
		want  string // substring expected in the error
	}{
		{"empty from", []Rule{{ReadSubstring: "", WriteSubstring: "x"}}, "rule #1: empty read_substring"},
		{"no-op", []Rule{{ReadSubstring: "abc", WriteSubstring: "abc"}}, "read_substring equals write_substring"},
		{"dup from", []Rule{{ReadSubstring: "abc", WriteSubstring: "x"}, {ReadSubstring: "ABC", WriteSubstring: "y"}}, "duplicate read_substring with rule #1"},
		{"dup alias", []Rule{{ReadSubstring: "a", WriteSubstring: "Z"}, {ReadSubstring: "b", WriteSubstring: "z"}}, "duplicate write_substring"},
		{"chain", []Rule{{ReadSubstring: "a", WriteSubstring: "b"}, {ReadSubstring: "b", WriteSubstring: "c"}}, "chained rules"},
		{"chain substring", []Rule{{ReadSubstring: "Ромашка", WriteSubstring: "Клиент-7"}, {ReadSubstring: "Клиент", WriteSubstring: "X"}}, "chained rules"},
	}
	for _, c := range cases {
		_, err := New(c.rules)
		if err == nil {
			t.Errorf("%s: expected error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not contain %q", c.name, err, c.want)
		}
		// The error must never echo rule contents.
		for _, r := range c.rules {
			if r.ReadSubstring != "" && r.ReadSubstring != "a" && r.ReadSubstring != "b" && r.ReadSubstring != "abc" && r.ReadSubstring != "ABC" &&
				strings.Contains(err.Error(), r.ReadSubstring) {
				t.Errorf("%s: error leaks keyword %q", c.name, r.ReadSubstring)
			}
		}
	}
}

func TestLoad_MissingFileAndYAML(t *testing.T) {
	if _, err := Load("no-such-file.yaml"); err == nil {
		t.Fatal("missing file must fail closed")
	}
	dir := t.TempDir()
	p := dir + "/rules.yaml"
	if err := writeFile(p, "rules:\n  - read_substring: x\n    write_substring: y\n"); err != nil {
		t.Fatal(err)
	}
	a, err := Load(p)
	if err != nil {
		t.Fatalf("valid file: %v", err)
	}
	if a.Len() != 1 || a.Apply("x") != "y" {
		t.Fatal("loaded rules must work")
	}
	if err := writeFile(p, "rules: [broken\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("broken YAML must fail closed")
	}
}

func TestEmptyRuleset(t *testing.T) {
	a := mustNew(t, nil)
	if a.Len() != 0 || a.Apply("s") != "s" || a.Unmap("s") != "s" {
		t.Fatal("empty ruleset must be a no-op")
	}
}
