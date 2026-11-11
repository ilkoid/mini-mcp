package pg

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"mini-mcp/internal/anon"

	"github.com/jackc/pgx/v5/pgtype"
)

// Common PostgreSQL type OIDs (pg_type). Only the ones that need special
// JSON handling are listed; unknown OIDs fall through to default encoding.
const (
	oidInt8        = 20   // bigint — MUST be JSON string (>2^53 loses precision in float64)
	oidNumeric     = 1700 // numeric/decimal — MUST be JSON string (same reason)
	oidTimestamp   = 1114 // timestamp without time zone
	oidTimestamptz = 1184 // timestamp with time zone
	oidDate        = 1082
	oidTime        = 1083
	oidTimetz      = 1266
	oidJSON        = 114
	oidJSONB       = 3802
	oidUUID        = 2950
	oidBytea       = 17
)

// rowEncoder applies the PG→JSON type mapping from design §4.7 per column.
// A fresh encoder is reused across rows of one result (OIDs don't change).
// guardHits counts cells redacted by the fragment/bytea guards so RunReadOnly
// can attach a notice (test report 2026-09-03, Д-1/Д-2).
type rowEncoder struct {
	asString  []bool // true for int8/numeric columns → encode as JSON string
	isJSON    []bool // true for json/jsonb columns → mask strings inside the document
	guardHits int
}

func newRowEncoder(oids []uint32) *rowEncoder {
	e := &rowEncoder{asString: make([]bool, len(oids)), isJSON: make([]bool, len(oids))}
	for i, oid := range oids {
		if oid == oidInt8 || oid == oidNumeric {
			e.asString[i] = true
		}
		if oid == oidJSON || oid == oidJSONB {
			e.isJSON[i] = true
		}
	}
	return e
}

// encode converts one row of pgx-returned values into a JSON-safe slice and
// also returns the byte size of the encoded row (for the byte cap).
func (e *rowEncoder) encode(values []any) ([]interface{}, int, error) {
	out := make([]interface{}, len(values))
	total := 3 // per-row JSON framing: '[', ']', and the ',' separator
	for i, v := range values {
		if e.isJSON[i] {
			if cell, n, ok := encodeJSONCell(v); ok {
				out[i] = cell
				total += n
				continue
			}
		}
		cell, n, guarded, err := encodeCell(v, e.asString[i])
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

// encodeJSONCell masks keywords inside a json/jsonb cell by walking the
// document (string values and map keys) instead of blind byte replacement,
// which could corrupt the JSON structure. ok=false falls back to the normal
// per-type encoding.
func encodeJSONCell(v any) (interface{}, int, bool) {
	var raw []byte
	switch x := v.(type) {
	case nil:
		return nil, 4, true
	case []byte:
		raw = x
	case json.RawMessage:
		raw = x
	case string:
		raw = []byte(x)
	default:
		return nil, 0, false
	}
	masked := Anonymizer.ApplyJSON(raw)
	return json.RawMessage(masked), len(masked), true
}

// encodeCell maps a single PG value to a JSON-safe Go value. Returns the
// value, its approximate JSON byte cost (best-effort, for the byte cap), and
// whether a guard redacted/rewrote it (for the guard notice). String cells
// are masked through the process-wide Anonymizer and then run through the
// fragment guard; numeric cells additionally match numeric keywords (INN-like)
// by their canonical string form and become the alias string.
func encodeCell(v any, asString bool) (interface{}, int, bool, error) {
	if v == nil {
		return nil, 4, false, nil // "null"
	}

	// BIGINT / numeric: emit as JSON string to dodge float64 precision loss.
	// pgx decodes int8 → int64; numeric arrives as a pgtype.Numeric VALUE (not
	// pointer) via rows.Values() — both must be handled or every computed
	// numeric (round(), numeric division, ::numeric) fails the whole query.
	if asString {
		cell, n, err := encodeNumericAsString(v)
		if err != nil {
			return nil, 0, false, err
		}
		if s, ok := cell.(string); ok {
			if alias, hit := Anonymizer.ApplyNumericString(s); hit {
				return alias, len(alias) + 2, false, nil
			}
		}
		return cell, n, false, nil
	}

	switch x := v.(type) {
	case string:
		// Text cells: mask before marshalling so JSON escaping stays correct,
		// then redact surviving keyword fragments (left()/reverse()-style
		// derivatives computed in the DB, test report Д-1).
		s := Anonymizer.Apply(x)
		guarded, hit := Anonymizer.GuardFragments(s)
		return guarded, len(guarded) + 2, hit, nil
	case int:
		if alias, ok := Anonymizer.ApplyNumericString(strconv.Itoa(x)); ok {
			return alias, len(alias) + 2, false, nil
		}
	case int32:
		if alias, ok := Anonymizer.ApplyNumericString(strconv.FormatInt(int64(x), 10)); ok {
			return alias, len(alias) + 2, false, nil
		}
	case int64:
		if alias, ok := Anonymizer.ApplyNumericString(strconv.FormatInt(x, 10)); ok {
			return alias, len(alias) + 2, false, nil
		}
	case float64:
		if alias, ok := Anonymizer.ApplyNumericString(strconv.FormatFloat(x, 'f', -1, 64)); ok {
			return alias, len(alias) + 2, false, nil
		}
	case time.Time:
		// Pin timestamps to RFC3339 (UTC) so they round-trip unambiguously.
		s := x.UTC().Format(time.RFC3339Nano)
		return s, len(s) + 2, false, nil
	case [16]byte:
		// uuid via rows.Values(): pgx hands a bare [16]byte — a DIFFERENT
		// type from []byte, so the bytea case doesn't catch it and default
		// json.Marshal would emit a JSON array of 16 byte numbers (live
		// finding, 2026-09-03). Render the canonical 8-4-4-4-12 string.
		s := Anonymizer.Apply(formatUUID(x))
		return s, len(s) + 2, false, nil
	case pgtype.UUID:
		// uuid in struct form (other code paths / future pgx): same fix.
		if !x.Valid {
			return nil, 4, false, nil
		}
		s := Anonymizer.Apply(formatUUID(x.Bytes))
		return s, len(s) + 2, false, nil
	case pgtype.Time:
		// time/timetz: pgtype.Time is {Microseconds, Valid}; default
		// marshalling leaks the struct shape as {"Microseconds":…} (live
		// finding, 2026-09-03). Render PG's text form HH:MM:SS[.FFFFFF].
		if !x.Valid {
			return nil, 4, false, nil
		}
		s := Anonymizer.Apply(formatPGTime(x.Microseconds))
		return s, len(s) + 2, false, nil
	case []byte:
		// bytea: pgx returns raw bytes. JSON-encode as base64 only if tiny;
		// otherwise reject (large bytea would blow the byte cap anyway).
		if len(x) > 1024 {
			return nil, 0, false, fmt.Errorf("bytea value too large (%d bytes) for read-only MCP output", len(x))
		}
		// Textual bytea guard (test report Д-2): brand::bytea /
		// convert_to(brand,'UTF8') would otherwise ship the real value as
		// opaque base64. Decode (UTF-8, or UTF-16 with BOM), mask + guard;
		// when the payload carries a keyword or fragment, return the MASKED
		// TEXT (or the redaction) instead of the encoding of the original.
		if txt, ok := anon.DecodeTextual(x); ok {
			masked := Anonymizer.Apply(txt)
			guarded, hit := Anonymizer.GuardFragments(masked)
			if hit {
				return guarded, len(guarded) + 2, true, nil
			}
			if masked != txt {
				return masked, len(masked) + 2, false, nil
			}
		}
		// json.Marshal of []byte → base64 string, which is what we want.
		b, err := json.Marshal(x)
		if err != nil {
			return nil, 0, false, err
		}
		return json.RawMessage(b), len(b), false, nil
	}

	// Default: rely on encoding/json. json.RawMessage values (e.g. decoded
	// json/jsonb) embed verbatim; numbers and bools marshal fine.
	b, err := json.Marshal(v)
	if err != nil {
		return nil, 0, false, err
	}
	return json.RawMessage(b), len(b), false, nil
}

// decodeTextual moved to the shared engine as anon.DecodeTextual (the MSSQL
// binary guard needs the identical UTF-8/UTF-16 decode).

// encodeNumericAsString forces int8/numeric into a JSON string value.
func encodeNumericAsString(v any) (interface{}, int, error) {
	switch x := v.(type) {
	case string:
		// Already textual (numeric can arrive as string) — keep verbatim, but
		// normalize whitespace.
		s := strings.TrimSpace(x)
		if !looksNumeric(s) {
			return nil, 0, fmt.Errorf("non-numeric value %q in numeric/int8 column", x)
		}
		return s, len(s) + 2, nil
	case int64:
		s := strconv.FormatInt(x, 10)
		return s, len(s) + 2, nil
	case int:
		s := strconv.Itoa(x)
		return s, len(s) + 2, nil
	case uint64:
		s := strconv.FormatUint(x, 10)
		return s, len(s) + 2, nil
	case float32:
		// numeric can decode to float32/float64 when there's no exact int fit.
		// Prefer the shortest round-trippable form as a string.
		s := strconv.FormatFloat(float64(x), 'g', -1, 64)
		return s, len(s) + 2, nil
	case float64:
		s := strconv.FormatFloat(x, 'g', -1, 64)
		return s, len(s) + 2, nil
	case bool:
		// odd, but keep truthful
		s := strconv.FormatBool(x)
		return s, len(s) + 2, nil
	case []byte:
		s := strings.TrimSpace(string(x))
		if !looksNumeric(s) {
			return nil, 0, fmt.Errorf("non-numeric value %q in numeric/int8 column", s)
		}
		return s, len(s) + 2, nil
	case *big.Int:
		s := x.String()
		return s, len(s) + 2, nil
	case pgtype.Numeric:
		return encodePgNumeric(x)
	case *pgtype.Numeric:
		if x == nil {
			return nil, 4, nil
		}
		return encodePgNumeric(*x)
	}
	// Fall back: stringify via fmt. Anything landing here is unexpected for an
	// int8/numeric column; surface it as an error rather than silently coerce.
	return nil, 0, fmt.Errorf("unexpected type %T for int8/numeric column", v)
}

// encodePgNumeric renders the pgtype.Numeric struct pgx hands us for numeric
// columns as an exact decimal string (never float64 — precision). NULL stays
// null; NaN/Infinity keep their PG spellings so the value remains truthful.
func encodePgNumeric(n pgtype.Numeric) (interface{}, int, error) {
	if !n.Valid {
		return nil, 4, nil // NULL
	}
	if n.NaN {
		s := "NaN"
		return s, len(s) + 2, nil
	}
	switch n.InfinityModifier {
	case pgtype.Infinity:
		s := "Infinity"
		return s, len(s) + 2, nil
	case pgtype.NegativeInfinity:
		s := "-Infinity"
		return s, len(s) + 2, nil
	}
	// driver.Valuer renders the exact text form via the numeric codec's text
	// format (plain decimal — PG numeric never uses e-notation on output).
	v, err := n.Value()
	if err != nil {
		return nil, 0, fmt.Errorf("render numeric: %w", err)
	}
	s, ok := v.(string)
	if !ok {
		return nil, 0, fmt.Errorf("non-numeric driver.Value %T for numeric column", v)
	}
	if !looksNumeric(strings.TrimSpace(s)) {
		return nil, 0, fmt.Errorf("non-numeric value %q in numeric/int8 column", s)
	}
	return s, len(s) + 2, nil
}

// formatUUID renders a 16-byte UUID in the canonical 8-4-4-4-12 text form
// (same shape pgx's encodeUUID / PG's uuid_out produce).
func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// formatPGTime renders microseconds-since-midnight as PG's time text shape
// HH:MM:SS[.FFFFFF], trimming a trailing zero fraction like PG does.
func formatPGTime(micro int64) string {
	h := micro / 3_600_000_000
	m := (micro / 60_000_000) % 60
	s := (micro / 1_000_000) % 60
	frac := micro % 1_000_000
	if frac == 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d.%s", h, m, s, strings.TrimRight(fmt.Sprintf("%06d", frac), "0"))
}

// looksNumeric is a cheap guard so we don't emit arbitrary strings into a
// column the schema declared numeric. Accepts optional leading sign, digits,
// and a single '.' plus optional exponent.
func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, ok := new(big.Rat).SetString(s)
	return ok
}

// errNotNumeric signals a value that isn't numeric-shaped (unused externally,
// kept for clarity in future validation).
var errNotNumeric = errors.New("value is not numeric")
