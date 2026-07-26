package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Common PostgreSQL type OIDs (pg_type). Only the ones that need special
// JSON handling are listed; unknown OIDs fall through to default encoding.
const (
	oidInt8       = 20 // bigint — MUST be JSON string (>2^53 loses precision in float64)
	oidNumeric    = 1700 // numeric/decimal — MUST be JSON string (same reason)
	oidTimestamp  = 1114 // timestamp without time zone
	oidTimestamptz = 1184 // timestamp with time zone
	oidDate       = 1082
	oidTime       = 1083
	oidTimetz     = 1266
	oidJSON       = 114
	oidJSONB      = 3802
	oidUUID       = 2950
	oidBytea      = 17
)

// rowEncoder applies the PG→JSON type mapping from design §4.7 per column.
// A fresh encoder is reused across rows of one result (OIDs don't change).
type rowEncoder struct {
	asString []bool // true for int8/numeric columns → encode as JSON string
}

func newRowEncoder(oids []uint32) *rowEncoder {
	e := &rowEncoder{asString: make([]bool, len(oids))}
	for i, oid := range oids {
		if oid == oidInt8 || oid == oidNumeric {
			e.asString[i] = true
		}
	}
	return e
}

// encode converts one row of pgx-returned values into a JSON-safe slice and
// also returns the byte size of the encoded row (for the byte cap).
func (e *rowEncoder) encode(values []any) ([]interface{}, int, error) {
	out := make([]interface{}, len(values))
	var total int
	for i, v := range values {
		cell, n, err := encodeCell(v, e.asString[i])
		if err != nil {
			return nil, 0, fmt.Errorf("column %d: %w", i, err)
		}
		out[i] = cell
		total += n
	}
	return out, total, nil
}

// encodeCell maps a single PG value to a JSON-safe Go value. Returns the
// value plus its approximate JSON byte cost (best-effort, for the byte cap).
func encodeCell(v any, asString bool) (interface{}, int, error) {
	if v == nil {
		return nil, 4, nil // "null"
	}

	// BIGINT / numeric: emit as JSON string to dodge float64 precision loss.
	// pgx already decodes int8 → int64 and numeric → *pgtype.Numeric; we also
	// guard against numeric coming back as string (some configs) or float.
	if asString {
		return encodeNumericAsString(v)
	}

	switch x := v.(type) {
	case time.Time:
		// Pin timestamps to RFC3339 (UTC) so they round-trip unambiguously.
		s := x.UTC().Format(time.RFC3339Nano)
		return s, len(s) + 2, nil
	case []byte:
		// bytea: pgx returns raw bytes. JSON-encode as base64 only if tiny;
		// otherwise reject (large bytea would blow the byte cap anyway).
		if len(x) > 1024 {
			return nil, 0, fmt.Errorf("bytea value too large (%d bytes) for read-only MCP output", len(x))
		}
		// json.Marshal of []byte → base64 string, which is what we want.
		b, err := json.Marshal(x)
		if err != nil {
			return nil, 0, err
		}
		return json.RawMessage(b), len(b), nil
	}

	// Default: rely on encoding/json. json.RawMessage values (e.g. decoded
	// json/jsonb) embed verbatim. Numbers, bools, strings, uuid-as-string all
	// marshal fine.
	b, err := json.Marshal(v)
	if err != nil {
		return nil, 0, err
	}
	return json.RawMessage(b), len(b), nil
}

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
	}
	// Fall back: stringify via fmt. Anything landing here is unexpected for an
	// int8/numeric column; surface it as an error rather than silently coerce.
	return nil, 0, fmt.Errorf("unexpected type %T for int8/numeric column", v)
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
