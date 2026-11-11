// encode.go — SQL Server→JSON type mapping per design §4.7 (T-SQL side).
//
// The go-mssqldb driver hands values over with these Go types (verified
// against v1.11.0 source): the whole INT family as int64, DECIMAL/NUMERIC/
// MONEY as []byte holding the EXACT decimal text (shopspring), BIT as bool,
// VARCHAR/NVARCHAR/TEXT/NTEXT/XML as string, VARBINARY/BINARY/IMAGE as
// []byte, UNIQUEIDENTIFIER as []byte(16) in TDS wire order, DATE/TIME/
// DATETIME*/DATETIMEOFFSET as time.Time. Column kinds are keyed on
// ColumnType().DatabaseTypeName() (the driver returns UPPERCASE names);
// unknown/empty names fall through to value-driven encoding.
package mssql

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mini-mcp/internal/anon"
)

type colKind int

const (
	kindAuto      colKind = iota // value-driven fallback
	kindBigint                   // BIGINT → JSON string (>2^53 loses precision in the client's float64)
	kindDecimal                  // DECIMAL/NUMERIC/MONEY/SMALLMONEY → exact string
	kindBinary                   // VARBINARY/BINARY/IMAGE → binary guard
	kindTextBytes                // TEXT/NTEXT/XML arriving as []byte → text masking
	kindTimeOfDay                // TIME → HH:MM:SS clock part
	kindGUID                     // UNIQUEIDENTIFIER → canonical 8-4-4-4-12 string
)

// rowEncoder applies the per-column type plan. guardHits counts cells
// redacted by the fragment/binary guards so RunReadOnly can attach a notice
// (test report 2026-09-03, Д-1/Д-2).
type rowEncoder struct {
	kinds     []colKind
	guardHits int
}

func newRowEncoder(typeNames []string) *rowEncoder {
	e := &rowEncoder{kinds: make([]colKind, len(typeNames))}
	for i, tn := range typeNames {
		switch strings.ToUpper(strings.TrimSpace(tn)) {
		case "BIGINT":
			e.kinds[i] = kindBigint
		case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
			e.kinds[i] = kindDecimal
		case "VARBINARY", "BINARY", "IMAGE":
			e.kinds[i] = kindBinary
		case "TEXT", "NTEXT", "XML":
			e.kinds[i] = kindTextBytes
		case "TIME":
			e.kinds[i] = kindTimeOfDay
		case "UNIQUEIDENTIFIER":
			e.kinds[i] = kindGUID
		}
	}
	return e
}

// scanTargets builds fresh *any destinations for one row (database/sql
// requires pointer destinations; the values are replaced per row).
func (e *rowEncoder) scanTargets() []any {
	dests := make([]any, len(e.kinds))
	for i := range dests {
		var v any
		dests[i] = &v
	}
	return dests
}

// encode converts one row of scanned destinations into a JSON-safe slice and
// returns its approximate byte size (for the cap).
func (e *rowEncoder) encode(dests []any) ([]interface{}, int, error) {
	values := make([]any, len(dests))
	for i, d := range dests {
		if p, ok := d.(*any); ok && p != nil {
			values[i] = *p
		}
	}
	out := make([]interface{}, len(values))
	total := 3 // per-row JSON framing: '[', ']', and the ',' separator
	for i, v := range values {
		cell, n, guarded, err := encodeCell(v, e.kinds[i])
		if err != nil {
			return nil, 0, fmt.Errorf("column %d: %w", i, err)
		}
		if guarded {
			e.guardHits++
		}
		out[i] = cell
		total += n
	}
	return out, total, nil
}

// encodeCell maps a single driver value to a JSON-safe Go value. Returns
// the value, its approximate JSON byte cost, and whether a guard
// redacted/rewrote it. Text cells are masked (Anonymizer.Apply) and then run
// through the fragment guard; numeric cells match numeric keywords by their
// canonical string form.
func encodeCell(v any, kind colKind) (interface{}, int, bool, error) {
	if v == nil {
		return nil, 4, false, nil // "null"
	}

	switch kind {
	case kindBigint:
		if s, ok := numericString(v); ok {
			if alias, hit := Anonymizer.ApplyNumericString(s); hit {
				return alias, len(alias) + 2, false, nil
			}
			return s, len(s) + 2, false, nil
		}
		return nil, 0, false, fmt.Errorf("unexpected type %T for bigint column", v)

	case kindDecimal:
		if s, ok := decimalString(v); ok {
			if alias, hit := Anonymizer.ApplyNumericString(s); hit {
				return alias, len(alias) + 2, false, nil
			}
			return s, len(s) + 2, false, nil
		}
		return nil, 0, false, fmt.Errorf("unexpected type %T for decimal/money column", v)

	case kindGUID:
		if b, ok := v.([]byte); ok && len(b) == 16 {
			s := Anonymizer.Apply(formatTDSGuid(b))
			return s, len(s) + 2, false, nil
		}
		if s, ok := v.(string); ok { // some drivers stringify GUIDs already
			s = Anonymizer.Apply(s)
			return s, len(s) + 2, false, nil
		}
		return nil, 0, false, fmt.Errorf("unexpected type %T for uniqueidentifier column", v)

	case kindBinary:
		return encodeBinaryCell(v)

	case kindTimeOfDay:
		if t, ok := v.(time.Time); ok {
			s := formatClock(t)
			return s, len(s) + 2, false, nil
		}
		// fall through to the value-driven path below
	}

	switch x := v.(type) {
	case string:
		// Text cells: mask before marshalling so JSON escaping stays
		// correct, then redact surviving keyword fragments (LEFT/REVERSE-
		// style derivatives computed in the DB, test report Д-1).
		s := Anonymizer.Apply(x)
		guarded, hit := Anonymizer.GuardFragments(s)
		return guarded, len(guarded) + 2, hit, nil

	case []byte:
		if kind == kindTextBytes {
			s := Anonymizer.Apply(string(x))
			guarded, hit := Anonymizer.GuardFragments(s)
			return guarded, len(guarded) + 2, hit, nil
		}
		// Unknown []byte column: textual payload → mask as text; a real
		// binary payload keeps the base64 path.
		return encodeBinaryCell(x)

	case int:
		if alias, ok := Anonymizer.ApplyNumericString(strconv.Itoa(x)); ok {
			return alias, len(alias) + 2, false, nil
		}
		return x, len(strconv.Itoa(x)), false, nil
	case int32:
		if alias, ok := Anonymizer.ApplyNumericString(strconv.FormatInt(int64(x), 10)); ok {
			return alias, len(alias) + 2, false, nil
		}
		return x, len(strconv.FormatInt(int64(x), 10)), false, nil
	case int64:
		s := strconv.FormatInt(x, 10)
		if alias, ok := Anonymizer.ApplyNumericString(s); ok {
			return alias, len(alias) + 2, false, nil
		}
		// JSON numbers parse as float64 client-side; beyond 2^53 the exact
		// integer must travel as a string (same reason as PG int8).
		if x > 4503599627370496 || x < -4503599627370496 {
			return s, len(s) + 2, false, nil
		}
		return x, len(s), false, nil
	case float64:
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if alias, ok := Anonymizer.ApplyNumericString(s); ok {
			return alias, len(alias) + 2, false, nil
		}
		return x, len(s), false, nil
	case bool:
		return x, 5, false, nil // "true"/"false"
	case time.Time:
		// Pin timestamps to RFC3339 so they round-trip unambiguously; keep
		// the original offset (DATETIMEOFFSET carries a real one, naive
		// types arrive in the session timezone).
		s := x.Format(time.RFC3339Nano)
		return s, len(s) + 2, false, nil
	}

	// Default: rely on encoding/json.
	b, err := json.Marshal(v)
	if err != nil {
		return nil, 0, false, err
	}
	return json.RawMessage(b), len(b), false, nil
}

// encodeBinaryCell guards binary columns (Д-2): CAST(col AS VARBINARY) /
// HASHBYTES output would otherwise ship the real value as opaque base64.
// Decode (UTF-8, or UTF-16 with BOM), mask + guard; when the payload
// carries a keyword or fragment, return the MASKED TEXT (or the redaction)
// instead of an encoding of the original. Large binaries are rejected —
// they would blow the byte cap anyway.
func encodeBinaryCell(v any) (interface{}, int, bool, error) {
	x, ok := v.([]byte)
	if !ok {
		return nil, 0, false, fmt.Errorf("unexpected type %T for binary column", v)
	}
	if len(x) > 1024 {
		return nil, 0, false, fmt.Errorf("binary value too large (%d bytes) for read-only MCP output", len(x))
	}
	if txt, ok := anon.DecodeTextual(x); ok {
		masked := Anonymizer.Apply(txt)
		guarded, hit := Anonymizer.GuardFragments(masked)
		if hit {
			return guarded, len(guarded) + 2, true, nil
		}
		// Decodable text (masking applied, fragments guarded — a no-op here
		// when nothing matched): ship as text. A varbinary cell holding
		// decodable text is almost always a cast probe; plain text output
		// is friendlier than base64 and equally safe after both guards.
		return masked, len(masked) + 2, false, nil
	}
	// json.Marshal of []byte → base64 string.
	b, err := json.Marshal(x)
	if err != nil {
		return nil, 0, false, err
	}
	return json.RawMessage(b), len(b), false, nil
}

// numericString renders integer-ish values (the whole INT family arrives as
// int64) as canonical text.
func numericString(v any) (string, bool) {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10), true
	case int:
		return strconv.Itoa(x), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	}
	return "", false
}

// decimalString renders DECIMAL/NUMERIC/MONEY values as exact text. The
// driver delivers them as []byte already holding the exact decimal string
// (shopspring); float64 only appears if a future driver version changes —
// formatted shortest-exact it is still the best available rendering.
func decimalString(v any) (string, bool) {
	switch x := v.(type) {
	case []byte:
		s := strings.TrimSpace(string(x))
		return s, looksNumeric(s)
	case string:
		s := strings.TrimSpace(x)
		return s, looksNumeric(s)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int64:
		return strconv.FormatInt(x, 10), true
	}
	return "", false
}

// looksNumeric is a cheap guard so arbitrary strings don't masquerade as
// decimals in a schema-declared decimal column.
func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' || s[0] == '+' {
		i++
	}
	digits, dot := false, false
	for ; i < len(s); i++ {
		switch {
		case s[i] >= '0' && s[i] <= '9':
			digits = true
		case s[i] == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digits
}

// formatTDSGuid renders the 16 raw TDS GUID bytes in the canonical
// 8-4-4-4-12 text form. TDS stores Data1/Data2/Data3 little-endian and
// Data4 big-endian (the .NET GUID layout), so the first three groups are
// byte-reversed for display.
func formatTDSGuid(b []byte) string {
	rev := func(s []byte) []byte {
		out := make([]byte, len(s))
		for i := range s {
			out[len(s)-1-i] = s[i]
		}
		return out
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", rev(b[0:4]), rev(b[4:6]), rev(b[6:8]), b[8:10], b[10:16])
}

// formatClock renders the clock part of the driver's TIME value (a full
// time.Time pinned to a dummy date) as HH:MM:SS[.ffffff], trimming the
// trailing zero fraction.
func formatClock(t time.Time) string {
	if f := t.Nanosecond(); f != 0 {
		s := t.Format("15:04:05.000000")
		return strings.TrimRight(s, "0")
	}
	return t.Format("15:04:05")
}
