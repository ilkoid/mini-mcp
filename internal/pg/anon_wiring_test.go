package pg

import (
	"encoding/json"
	"strings"
	"testing"

	"mini-mcp/internal/anon"
	"mini-mcp/internal/result"
)

// oidText is the PG text type — plain string cells.
const oidText = 25

// setAnonymizer installs a synthetic masker for one test and restores the
// package state afterwards. Synthetic markers, never real keywords.
func setAnonymizer(t *testing.T) {
	t.Helper()
	a, err := anon.New([]anon.Rule{{ReadSubstring: "__secret_word__", WriteSubstring: "__alias__"}})
	if err != nil {
		t.Fatal(err)
	}
	Anonymizer = a
	t.Cleanup(func() { Anonymizer = nil })
}

func TestEncodeCell_MasksStringCells(t *testing.T) {
	setAnonymizer(t)
	e := newRowEncoder([]uint32{oidText})
	row, _, err := e.encode([]any{"shown __secret_word__ hidden"})
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != "shown __alias__ hidden" {
		t.Fatalf("string cell not masked: %v", row[0])
	}
}

func TestEncodeCell_MasksInt8ByNumericRule(t *testing.T) {
	a, err := anon.New([]anon.Rule{{ReadSubstring: "7701234567", WriteSubstring: "INN-X"}})
	if err != nil {
		t.Fatal(err)
	}
	Anonymizer = a
	t.Cleanup(func() { Anonymizer = nil })

	e := newRowEncoder([]uint32{oidInt8})
	row, _, err := e.encode([]any{int64(7701234567)})
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != "INN-X" {
		t.Fatalf("numeric keyword not masked: %v", row[0])
	}
	// A different number passes through as the exact string.
	row2, _, err := e.encode([]any{int64(42)})
	if err != nil {
		t.Fatal(err)
	}
	if row2[0] != "42" {
		t.Fatalf("unrelated int8 mangled: %v", row2[0])
	}
}

func TestEncodeJSONCell_MasksInsideDocument(t *testing.T) {
	setAnonymizer(t)
	e := newRowEncoder([]uint32{oidJSONB})
	row, _, err := e.encode([]any{[]byte(`{"note":"value __secret_word__ end","n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := row[0].(json.RawMessage)
	if !ok {
		t.Fatalf("jsonb cell is %T, want json.RawMessage", row[0])
	}
	// Masking walks the document via map[string]any, so key ORDER may change
	// (Go marshals map keys sorted) — compare structurally.
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("masked jsonb is not valid JSON: %s", raw)
	}
	if got["note"] != "value __alias__ end" {
		t.Fatalf("jsonb value not masked: %s", raw)
	}
	if got["n"] != float64(1) {
		t.Fatalf("unrelated jsonb number mangled: %s", raw)
	}
	if s := string(raw); strings.Contains(s, "__secret_word__") {
		t.Fatalf("jsonb masking leaked the keyword: %s", s)
	}
}

func TestColumnNamesAndNoticeMasked(t *testing.T) {
	setAnonymizer(t)
	// truncationNotice is static, but the masking call site must not choke.
	n := Anonymizer.Apply(result.TruncationNotice(maxBytes + 1))
	if n == "" {
		t.Fatal("notice masking produced empty text")
	}
}

func TestToolErrorTextMasked(t *testing.T) {
	setAnonymizer(t)
	err := &fakeError{msg: "dup key (__secret_word__) violates"}
	got := Anonymizer.ApplyErr(err)
	if got.Error() != "dup key (__alias__) violates" {
		t.Fatalf("error text not masked: %q", got.Error())
	}
}

type fakeError struct{ msg string }

func (f *fakeError) Error() string { return f.msg }
