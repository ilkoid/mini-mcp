package anon

import (
	"unicode/utf16"
	"unicode/utf8"
)

// DecodeTextual decodes b as UTF-8 (or UTF-16 with a BOM — what
// convert_to(text,'UTF16') produces on PG, and what NVARCHAR reinterpreted
// as varbinary looks like on MSSQL). It backs the binary guards in both
// dialect packages: a textual payload inside a binary cell must go through
// Apply/GuardFragments, not ship as opaque base64 (test report 2026-09-03,
// Д-2). ok=false for anything else; binary payloads then keep the base64
// path in the callers.
func DecodeTextual(b []byte) (string, bool) {
	if utf8.Valid(b) {
		return string(b), true
	}
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE { // UTF-16LE BOM
		u := make([]uint16, 0, (len(b)-2)/2)
		for i := 2; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return string(utf16.Decode(u)), true
	}
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF { // UTF-16BE BOM
		u := make([]uint16, 0, (len(b)-2)/2)
		for i := 2; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		}
		return string(utf16.Decode(u)), true
	}
	return "", false
}
