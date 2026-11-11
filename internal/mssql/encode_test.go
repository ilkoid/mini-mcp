package mssql

import (
	"encoding/json"
	"testing"
	"time"

	"mini-mcp/internal/anon"
)

func TestEncodeCell_MasksAndGuardsText(t *testing.T) {
	setAnonymizer(t)
	cell, n, guarded, err := encodeCell("shown SampleWord hidden", kindAuto)
	if err != nil {
		t.Fatal(err)
	}
	if cell != "shown AliasWord hidden" || guarded {
		t.Fatalf("text cell not masked cleanly: %v guarded=%v", cell, guarded)
	}
	// A LEFT(col,4)-style derivative surviving from the DB is redacted.
	cell, _, guarded, err = encodeCell("Samp fragment", kindAuto)
	if err != nil {
		t.Fatal(err)
	}
	if cell != "… fragment" || !guarded {
		t.Fatalf("fragment not guarded: %v guarded=%v", cell, guarded)
	}
	_ = n
}

func TestEncodeCell_NumericKinds(t *testing.T) {
	setAnonymizer(t)
	// BIGINT → JSON string (2^53 safety), numeric keywords masked.
	cell, _, _, err := encodeCell(int64(9223372036854775807), kindBigint)
	if err != nil || cell != "9223372036854775807" {
		t.Fatalf("bigint: %v %v", cell, err)
	}
	a, _ := anon.New([]anon.Rule{{ReadSubstring: "7701234567", WriteSubstring: "INN-X"}})
	Anonymizer = a
	t.Cleanup(func() { Anonymizer = nil })
	cell, _, _, err = encodeCell(int64(7701234567), kindBigint)
	if err != nil || cell != "INN-X" {
		t.Fatalf("numeric keyword not masked: %v %v", cell, err)
	}
	// DECIMAL arrives from the driver as []byte exact text.
	cell, _, _, err = encodeCell([]byte("12345.67"), kindDecimal)
	if err != nil || cell != "12345.67" {
		t.Fatalf("decimal text: %v %v", cell, err)
	}
	// Defensive float64 path formats shortest-exact.
	cell, _, _, err = encodeCell(float64(1.23), kindDecimal)
	if err != nil || cell != "1.23" {
		t.Fatalf("decimal float: %v %v", cell, err)
	}
	// INT in kindAuto stays a JSON number; a huge int64 in kindAuto
	// (missing type info) degrades to a string, not float-corrupted JSON.
	cell, _, _, err = encodeCell(int64(42), kindAuto)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cell.(int64); !ok {
		t.Fatalf("small int should stay numeric, got %T", cell)
	}
	cell, _, _, err = encodeCell(int64(9223372036854775807), kindAuto)
	if err != nil || cell != "9223372036854775807" {
		t.Fatalf("huge auto int64: %v %v", cell, err)
	}
}

func TestEncodeCell_Times(t *testing.T) {
	setAnonymizer(t)
	ts := time.Date(2026, 9, 3, 12, 34, 56, 0, time.UTC)
	cell, _, _, err := encodeCell(ts, kindAuto)
	if err != nil || cell != "2026-09-03T12:34:56Z" {
		t.Fatalf("datetime: %v %v", cell, err)
	}
	// TIME: only the clock part, fraction trimmed like PG's time output.
	tod := time.Date(1900, 1, 1, 12, 34, 56, 500000000, time.UTC)
	cell, _, _, err = encodeCell(tod, kindTimeOfDay)
	if err != nil || cell != "12:34:56.5" {
		t.Fatalf("time of day: %v %v", cell, err)
	}
}

func TestEncodeCell_GUID(t *testing.T) {
	setAnonymizer(t)
	// TDS wire order: Data1/Data2/Data3 little-endian, Data4 big-endian.
	tds := []byte{0x33, 0x22, 0x11, 0x00, 0x55, 0x44, 0x77, 0x66,
		0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	cell, _, _, err := encodeCell(tds, kindGUID)
	if err != nil {
		t.Fatal(err)
	}
	if cell != "00112233-4455-6677-8899-aabbccddeeff" {
		t.Fatalf("guid formatting: %v", cell)
	}
}

func TestEncodeCell_BinaryGuard(t *testing.T) {
	setAnonymizer(t)
	// utf-8 keyword inside varbinary → masked text, not base64.
	cell, _, guarded, err := encodeCell([]byte("x SampleWord y"), kindBinary)
	if err != nil || cell != "x AliasWord y" || guarded {
		t.Fatalf("binary guard mask: %v %v guarded=%v", cell, err, guarded)
	}
	// surviving fragment → redaction with the guard flag set.
	cell, _, guarded, err = encodeCell([]byte("Samp"), kindBinary)
	if err != nil || cell != "…" || !guarded {
		t.Fatalf("binary guard fragment: %v %v guarded=%v", cell, err, guarded)
	}
	// UTF-16LE with BOM decodes and masks.
	u16 := []byte{0xFF, 0xFE}
	for _, r := range "SampleWord" {
		u16 = append(u16, byte(r), 0)
	}
	cell, _, _, err = encodeCell(u16, kindBinary)
	if err != nil || cell != "AliasWord" {
		t.Fatalf("utf16 guard: %v %v", cell, err)
	}
	// Raw binary stays base64.
	cell, _, _, err = encodeCell([]byte{0x00, 0x01, 0xFF}, kindBinary)
	if err != nil {
		t.Fatal(err)
	}
	rm, ok := cell.(json.RawMessage)
	if !ok || string(rm) != `"AAH/"` {
		t.Fatalf("raw binary not base64: %v", cell)
	}
	// Oversized binary rejected.
	if _, _, _, err = encodeCell(make([]byte, 1025), kindBinary); err == nil {
		t.Fatal("oversized binary accepted")
	}
}

func TestEncodeRow_FramingAndGuardHits(t *testing.T) {
	setAnonymizer(t)
	e := newRowEncoder([]string{"VARCHAR", "Samp"}) // second col name unknown → kindAuto
	dests := e.scanTargets()
	*(dests[0].(*any)) = any("SampleWord text")
	*(dests[1].(*any)) = any("plain")
	row, chunk, err := e.encode(dests)
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != "AliasWord text" || row[1] != "plain" {
		t.Fatalf("row encoding: %v", row)
	}
	b, _ := json.Marshal(row)
	if chunk < len(b) {
		t.Fatalf("chunk %d underestimates wire size %d", chunk, len(b))
	}
	if e.guardHits != 0 {
		t.Fatalf("unexpected guard hits: %d", e.guardHits)
	}
	// Guarded cell bumps the counter.
	dests = e.scanTargets()
	*(dests[0].(*any)) = any("Samp")
	*(dests[1].(*any)) = any("SampleWord")
	if _, _, err = e.encode(dests); err != nil {
		t.Fatal(err)
	}
	if e.guardHits != 1 {
		t.Fatalf("guard hits: %d", e.guardHits)
	}
}
