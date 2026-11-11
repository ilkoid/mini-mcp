// precheck.go — T-SQL single-statement policy and anonymization-guard
// prechecks (the DX precheck; the role ACLs are the boundary).
package mssql

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"mini-mcp/internal/anon"
)

// nonReadKeywords are rejected up-front as DX (NOT a security boundary — the
// ai_reader role grants are the boundary). T-SQL specifics vs the PG list:
// EXEC/EXECUTE and any leading sp_* call (procedures can write through
// internal SQL we cannot see), DECLARE (variables enable multi-step
// payloads), USE (database switch), GO (batch separator), and the
// ad-hoc rowset providers (OPENROWSET & friends).
var nonReadKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "TRUNCATE", "MERGE",
	"ALTER", "DROP", "CREATE", "GRANT", "REVOKE", "DENY",
	"BACKUP", "RESTORE", "DBCC", "BULK",
	// procedural / batch / session management:
	"EXEC", "EXECUTE", "DECLARE", "SET", "USE", "GO",
	"BEGIN", "COMMIT", "ROLLBACK", "SAVE",
	"PRINT", "RAISERROR", "THROW", "WAITFOR",
	"OPENROWSET", "OPENQUERY", "OPENDATASOURCE", "OPENXML",
}

// trailingSemicolonRe matches a single trailing ';' possibly followed by space.
var trailingSemicolonRe = regexp.MustCompile(`;\s*$`)

// defaultBlockedFunctions is the T-SQL dialect list of functions rejected by
// the precheck because they emit DERIVATIVES of their arguments (substrings,
// hashes, encodings, lengths — test report 2026-09-03, Д-1/Д-2/Д-3; the PG
// sibling list lives in internal/pg/ro.go). Blocking is query-wide; the
// operator can replace the list via security.blocked_functions_override in
// the rules file.
var defaultBlockedFunctions = []string{
	// substring / extraction family (Д-1)
	"left", "right", "substring", "substr", "stuff", "reverse",
	"translate", "trim", "ltrim", "rtrim", "replace",
	// boolean oracles over content (Д-1)
	"charindex", "patindex", "soundex", "difference",
	// string splitting / re-shaping
	"string_split", "string_escape", "format", "str",
	// hash / encoding family (Д-2)
	"hashbytes", "compress", "decompress",
	"encryptbykey", "decryptbykey", "encryptbypassphrase", "decryptbypassphrase",
	"pwdencrypt", "pwdcompare",
	// length fingerprint family (Д-3)
	"len", "datalength",
}

// varbinaryCastRe catches the CAST/CONVERT-to-binary leak (Д-2): a textual
// value reinterpreted as VARBINARY would ship as opaque bytes and dodge the
// string masking path (the output-side binary guard covers the rest).
var varbinaryCastRe = regexp.MustCompile(`(?i)\b(as\s+varbinary|as\s+binary|convert\s*\(\s*varbinary|convert\s*\(\s*binary)\b`)

// RejectUnsafeSQL enforces the single-statement policy:
//   - reject empty / whitespace-only;
//   - reject more than one statement (any real ';' before the optional
//     trailing one — semicolons inside '…' / N'…' literals, [bracket
//     identifiers] and comments are data, not separators);
//   - reject leading non-read keywords (incl. sp_* procedure calls) as DX;
//   - reject blocked leak functions and CAST/CONVERT-to-binary as a DX
//     precheck for the anonymization guard.
//
// Intentionally NOT the security boundary — a payload that slipped past is
// still stopped by the ai_reader role ACLs.
func RejectUnsafeSQL(sql string) error {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return errors.New("empty SQL")
	}

	// Judge structure on the masked copy: literals/brackets/comments are
	// blanked so a ';' inside 'a;b' or [a;b] isn't a separator.
	masked := maskLiteralsAndComments(trimmed)

	// Single statement: strip ONE optional trailing ';', then any remaining
	// real ';' means multiple statements (agents must use @p1..@n params,
	// not inline values).
	body := trailingSemicolonRe.ReplaceAllString(masked, "")
	if strings.ContainsRune(body, ';') {
		return errors.New("multiple statements are not allowed; pass a single T-SQL statement (use @p1,@p2 for values)")
	}

	// GO batch separator: a standalone GO word on its own line (the masked
	// copy keeps literals/brackets blanked, so a "GO" inside data doesn't
	// trip this). Via the driver a GO batch is a syntax error at best.
	for _, line := range strings.Split(body, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "GO") {
			return errors.New("multiple statements are not allowed; GO batch separators are not accepted (pass a single T-SQL statement)")
		}
	}

	// Leading-keyword precheck. Identify the first word and reject if non-read.
	kw := leadingKeyword(body)
	for _, bad := range nonReadKeywords {
		if kw == bad {
			return fmt.Errorf("non-read statement %q rejected by precheck (this server is read-only)", kw)
		}
	}
	if strings.HasPrefix(kw, "SP_") {
		return fmt.Errorf("stored-procedure execution (%s…) rejected by precheck (this server is read-only)", kw)
	}

	// Anonymization guard: functions that emit derivatives of their
	// arguments (LEFT/HASHBYTES/LEN/…) would bypass full-keyword masking.
	if fn, blocked := anon.CheckBlockedFunctions(masked, Anonymizer.BlockedFunctions(defaultBlockedFunctions)); blocked {
		return fmt.Errorf("function %q is blocked by the anonymization guard: it can emit derivatives (substrings/hashes/lengths) of sensitive values — select the column itself or use aggregates; the operator can adjust the list via the rules file", fn)
	}
	if loc := varbinaryCastRe.FindString(masked); loc != "" {
		return fmt.Errorf("CAST/CONVERT to BINARY/VARBINARY is blocked by the anonymization guard: it can ship an encoding of a sensitive value — select the column itself")
	}
	return nil
}

// leadingKeyword returns the uppercase first keyword of the statement, with
// parentheses stripped so "(SELECT ...)" still classifies as SELECT.
func leadingKeyword(sql string) string {
	s := strings.TrimLeftFunc(sql, func(r rune) bool {
		return unicode.IsSpace(r) || r == '('
	})
	i := 0
	for i < len(s) {
		r := rune(s[i])
		if r == '(' || unicode.IsSpace(r) {
			break
		}
		i++
	}
	return strings.ToUpper(s[:i])
}

// maskLiteralsAndComments returns a copy of sql, same length, with every byte
// of a string literal ('…' and N'…' with ” escaping), bracket identifier
// [……] with ]] escaping, double-quoted identifier "…" with "" escaping, or
// comment — delimiters included — replaced by a space. Only syntax structure
// survives. T-SQL differences from the PG variant: bracket identifiers (may
// contain ';'), no dollar-quotes, no E” backslash escapes.
func maskLiteralsAndComments(sql string) string {
	b := []byte(sql)
	n := len(b)
	blank := func(from, to int) {
		for j := from; j < to && j < n; j++ {
			b[j] = ' '
		}
	}

	scanDelim := func(i int, close byte) int {
		// Returns index just past the closing delimiter (or n when
		// unterminated). Doubled closing bytes ('' and ]]) are escapes.
		j := i + 1
		for j < n {
			if b[j] == close {
				if j+1 < n && b[j+1] == close {
					j += 2
					continue
				}
				j++
				break
			}
			j++
		}
		return j
	}

	i := 0
	for i < n {
		c := b[i]
		switch {
		case c == '-' && i+1 < n && b[i+1] == '-':
			// Line comment: to end of line (the \n itself stays).
			j := i
			for j < n && b[j] != '\n' {
				j++
			}
			blank(i, j)
			i = j

		case c == '/' && i+1 < n && b[i+1] == '*':
			// Block comment, nestable in T-SQL.
			depth, j := 1, i+2
			for j < n && depth > 0 {
				switch {
				case b[j] == '/' && j+1 < n && b[j+1] == '*':
					depth++
					j += 2
				case b[j] == '*' && j+1 < n && b[j+1] == '/':
					depth--
					j += 2
				default:
					j++
				}
			}
			blank(i, j)
			i = j

		case c == '\'' || c == '[' || c == '"':
			// Quotes are self-closing delimiters; a T-SQL [bracket
			// identifier] OPENS with '[' and CLOSES with ']' — scanning for
			// the next '[' instead would swallow everything up to a LATER
			// bracket, hiding function names from the blocked-function guard
			// (live leak 2026-09-04: `..., HASHBYTES('MD5', [Марка])` shipped
			// real hashes because HASHBYTES sat inside the "literal" span).
			close := c
			if c == '[' {
				close = ']'
			}
			end := scanDelim(i, close)
			blank(i, end)
			i = end

		case (c == 'N' || c == 'n') && i+1 < n && b[i+1] == '\'' && nPrefixStartsToken(b, i):
			// National string N'…': mask the N together with the literal.
			end := scanDelim(i+1, '\'')
			blank(i, end)
			i = end

		default:
			i++
		}
	}
	return string(b)
}

// nPrefixStartsToken reports whether the N/n at b[i] is directly followed by
// a single quote AND starts its own token (not glued to another identifier
// like `colN'…'`, which is invalid/different SQL anyway).
func nPrefixStartsToken(b []byte, i int) bool {
	if i == 0 {
		return true
	}
	p := b[i-1]
	switch {
	case p >= 'a' && p <= 'z', p >= 'A' && p <= 'Z', p >= '0' && p <= '9',
		p == '_', p == '$', p == '"', p == ']':
		return false
	}
	return true
}
