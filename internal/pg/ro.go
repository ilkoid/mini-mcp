// Package pg — read-only query core for mcp-pg-readonly.
//
// This file is the security boundary's runtime side (the database role is the
// primary boundary — see role-bootstrap.sql). RunReadOnly wraps every agent
// SQL in a READ ONLY transaction with statement_timeout and output caps, and
// encodes results so that BIGINT/numeric survive the JSON float64 round-trip
// (WB nm_id values exceed 2^53). See design doc §4.2–4.7.
//
// Anonymization: every string that leaves this package goes through the
// process-wide Anonymizer (Apply direction); every SQL text / string bind
// value from the agent goes through Unmap (alias -> real value) BEFORE
// execution. The agent only ever knows aliases, the database only ever sees
// real values, and the session never observes the originals in either
// direction.
package pg

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mini-mcp/internal/anon"
	"mini-mcp/internal/result"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Anonymizer is the process-wide keyword-masking engine. nil = pass-through
// (unit tests default); cmd/mcp-pg-readonly requires --anon-config and sets
// this before serving, so a production entry always masks.
var Anonymizer *anon.Anonymizer

// Result is the shared JSON envelope (internal/result) — alias kept so the
// package API and tests stay unchanged after the extraction.
type Result = result.Result

// Output caps live in internal/result (shared with the MSSQL sibling).
const (
	maxRows  = result.MaxRows
	maxBytes = result.MaxBytes
)

// Per-query safety knobs (design §4.2). Vars (not consts) so
// cmd/mcp-pg-readonly can set them from --statement-timeout /
// --query-timeout flags. Defaults raised from 30s/60s to 120s/180s:
// analytical queries over WB fact tables routinely outgrow 30s, and the ctx
// deadline keeps 60s headroom over statement_timeout so the DB-side cancel
// (clean 57014) fires before the ctx cut.
var (
	QueryTimeout     = 180 * time.Second // ctx deadline wrapping the whole call
	StatementTimeout = 120 * time.Second // SET LOCAL statement_timeout inside the RO txn
)

// nonReadKeywords are rejected up-front as DX (NOT a security boundary — the
// role + READ ONLY txn are the boundary). Matching the leading keyword keeps
// the agent from getting a confusing role-denied error for an obvious typo.
// SHOW and EXPLAIN are deliberately NOT listed: both are read-only and useful
// to the agent. SET is transactional in PG (the always-rolled-back RO txn
// would undo it anyway), but letting it "succeed" silently with an empty
// result confused agents — reject it up-front instead.
var nonReadKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "TRUNCATE", "MERGE",
	"ALTER", "DROP", "CREATE", "GRANT", "REVOKE",
	"COPY", "CALL", "DO", "VACUUM", "REINDEX", "ANALYZE",
	"CLUSTER", "REFRESH", "COMMENT",
	// Session / statement / transaction management:
	"SET", "RESET", "DISCARD", "PREPARE", "EXECUTE", "DEALLOCATE",
	"LISTEN", "NOTIFY", "UNLISTEN", "LOCK", "CHECKPOINT",
	"SAVEPOINT", "RELEASE", "BEGIN", "START", "COMMIT",
	"ROLLBACK", "END", "ABORT", "DECLARE", "FETCH", "MOVE", "CLOSE",
}

// trailingSemicolonRe matches a single trailing ';' possibly followed by space.
var trailingSemicolonRe = regexp.MustCompile(`;\s*$`)

// defaultBlockedFunctions is the PG dialect list of functions rejected by the
// precheck because they emit DERIVATIVES of their arguments (substrings,
// hashes, encodings, lengths — test report 2026-09-03, Д-1/Д-2/Д-3): masking
// only replaces full keyword occurrences, so left(col,4) or md5(col) would
// ship the derivative untouched. Blocking is query-wide — without a
// column→dictionary mapping we cannot tell a sensitive column from an innocent
// one, so legit uses (length() on search_text) are traded away deliberately;
// the operator can replace the list via security.blocked_functions_override
// in the rules file. Residual: boolean oracles (strpos/position are listed,
// but CASE WHEN col LIKE '_l__' survives) — covered output-side by
// GuardFragments and documented as a limitation.
var defaultBlockedFunctions = []string{
	// substring / extraction family (Д-1)
	"left", "right", "substring", "substr", "split_part", "string_to_array",
	"btrim", "ltrim", "rtrim", "trim", "overlay", "lpad", "rpad",
	"regexp_replace", "regexp_count", "regexp_matches", "regexp_match",
	"regexp_split_to_array", "regexp_split_to_table",
	"translate", "reverse", "initcap", "ascii", "unicode",
	// boolean oracles over content (Д-1)
	"strpos", "position",
	// hash / encoding family (Д-2)
	"md5", "sha224", "sha256", "sha384", "sha512",
	"hmac", "digest", "crypt", "encode", "convert_to",
	// length fingerprint family (Д-3)
	"length", "char_length", "character_length", "octet_length", "bit_length",
}

// RejectUnsafeSQL enforces the single-statement policy (design §4.4):
//   - reject empty / whitespace-only;
//   - reject more than one statement (any real ';' before the optional
//     trailing one — semicolons inside literals/dollar-quotes/comments are
//     data, not separators);
//   - reject leading non-read keywords as a DX precheck;
//   - reject blocked leak functions (substring/hash/length families) as a DX
//     precheck for the anonymization guard.
//
// This is intentionally NOT the security boundary — a malicious multi-statement
// payload that slipped past would still be stopped by the READ ONLY transaction
// and the mcp_ro role ACLs.
func RejectUnsafeSQL(sql string) error {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return errors.New("empty SQL")
	}

	// Judge structure on the masked copy: literals/dollar-quotes/comments are
	// blanked so a ';' inside 'a;b' isn't a separator and a leading "-- note
	// \nDROP" still classifies by its real keyword.
	masked := maskLiteralsAndComments(trimmed)

	// Single statement: strip ONE optional trailing ';', then any remaining
	// real ';' means multiple statements (agents should use $1..$n params,
	// not inline values).
	body := trailingSemicolonRe.ReplaceAllString(masked, "")
	if strings.ContainsRune(body, ';') {
		return errors.New("multiple statements are not allowed; pass a single SQL statement (use $1,$2 for values)")
	}

	// Leading-keyword precheck. Identify the first word and reject if non-read.
	kw := leadingKeyword(body)
	for _, bad := range nonReadKeywords {
		if kw == bad {
			return fmt.Errorf("non-read statement %q rejected by precheck (this server is read-only)", kw)
		}
	}

	// Anonymization guard: functions that emit derivatives of their
	// arguments (left/md5/length/…) would bypass full-keyword masking.
	if fn, blocked := anon.CheckBlockedFunctions(masked, Anonymizer.BlockedFunctions(defaultBlockedFunctions)); blocked {
		return fmt.Errorf("function %q is blocked by the anonymization guard: it can emit derivatives (substrings/hashes/lengths) of sensitive values — select the column itself or use aggregates; the operator can adjust the list via the rules file", fn)
	}
	return nil
}

// leadingKeyword returns the uppercase first keyword of the statement, with
// parentheses/quotes stripped so "(SELECT ...)" still classifies as SELECT.
func leadingKeyword(sql string) string {
	s := strings.TrimLeftFunc(sql, func(r rune) bool {
		return unicode.IsSpace(r) || r == '('
	})
	// Stop at whitespace or '(' — a keyword is a contiguous run of letters/_/$.
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
// of a string literal, escape-string literal, quoted identifier, dollar-quoted
// body, or comment — delimiters included — replaced by a space. Only syntax
// structure survives, so the ';' and leading-keyword checks see real statement
// shape, not data. Placeholder params ($1, $2, …) are never dollar-quote
// openings (a tag cannot start with a digit) and stay visible.
func maskLiteralsAndComments(sql string) string {
	b := []byte(sql)
	n := len(b)
	blank := func(from, to int) {
		for j := from; j < to && j < n; j++ {
			b[j] = ' '
		}
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
			// Block comment, nestable per the SQL standard.
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

		case c == '\'' || c == '"':
			// Standard string / quoted identifier: only a doubled delimiter
			// escapes (standard_conforming_strings=on — backslash is data).
			delim := c
			j := i + 1
			for j < n {
				if b[j] == delim {
					if j+1 < n && b[j+1] == delim {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			blank(i, j)
			i = j

		case (c == 'e' || c == 'E') && escapeStringPrefixAt(b, i):
			// E'...' escape string: backslash escapes a delimiter.
			j := i + 2
			for j < n {
				if b[j] == '\\' {
					j += 2
					continue
				}
				if b[j] == '\'' {
					if j+1 < n && b[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			blank(i, j)
			i = j

		case c == '$' && dollarQuoteEnd(b, i) > i:
			// Dollar quote $tag$...$tag$ — the tag may be empty ($$) or start
			// with a letter/underscore, so $1-style params never match.
			tagEnd := dollarQuoteEnd(b, i)
			close := sql[i : tagEnd+1] // includes both '$' delimiters
			end := strings.Index(sql[tagEnd+1:], close)
			if end < 0 {
				blank(i, n) // unterminated: mask to the end, PG errors anyway
				i = n
			} else {
				stop := tagEnd + 1 + end + len(close)
				blank(i, stop)
				i = stop
			}

		default:
			i++
		}
	}
	return string(b)
}

// escapeStringPrefixAt reports whether the e/E at b[i] is directly followed
// by a single quote AND starts its own token (not glued to another identifier
// like `xe'…'`, which PG lexes as identifier + plain string).
func escapeStringPrefixAt(b []byte, i int) bool {
	if i+1 >= len(b) || b[i+1] != '\'' {
		return false
	}
	if i == 0 {
		return true
	}
	p := b[i-1]
	switch {
	case p >= 'a' && p <= 'z', p >= 'A' && p <= 'Z', p >= '0' && p <= '9',
		p == '_', p == '$', p == '"':
		return false
	}
	return true
}

// dollarQuoteEnd returns the index of the closing '$' of a dollar-quote
// opening at b[i], or i when b[i] is not a dollar-quote opening ($1 params
// and $ followed by a digit are not openings — tags cannot start with a digit).
func dollarQuoteEnd(b []byte, i int) int {
	n := len(b)
	if i >= n || b[i] != '$' {
		return i
	}
	j := i + 1
	if j < n && (isTagStart(b[j])) {
		j++
		for j < n && isTagCont(b[j]) {
			j++
		}
	}
	if j < n && b[j] == '$' {
		return j
	}
	return i
}

func isTagStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isTagCont(c byte) bool {
	return isTagStart(c) || c >= '0' && c <= '9'
}

// wrapQueryError translates timeout failures into actionable messages (design
// §4.2). SQLSTATE 57014 = query_canceled by statement_timeout; a raw ctx
// deadline means the wider --query-timeout fired first. Other errors pass
// through unchanged.
func wrapQueryError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return fmt.Errorf("query exceeded statement_timeout (%s); add filters/limits/aggregates or raise --statement-timeout: %w", StatementTimeout, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("query exceeded query-timeout (%s); raise --query-timeout (must stay above --statement-timeout): %w", QueryTimeout, err)
	}
	return err
}

// RunReadOnly executes a single SQL statement in a READ ONLY transaction with
// a statement_timeout (SET LOCAL), always rolls back, and applies row/byte caps.
// Anonymization happens here on both ends: Unmap on the way in (agent aliases
// become real values before the database sees them, then full-literal alias
// filters expand to all stored spellings via eq_any — two-era data), Apply on
// the way out (column names, notices) — cell values are masked and
// fragment-guarded by the row encoder.
func RunReadOnly(ctx context.Context, pool *pgxpool.Pool, sql string, args []any) (*Result, error) {
	// Pre-Unmap snapshot: the zero-rows and partial-pattern notices must judge
	// the SQL the AGENT wrote (aliases), not the restored keywords.
	originalSQL := sql
	var originalArgs []any
	if args != nil {
		originalArgs = append([]any{}, args...)
	}

	sql = Anonymizer.Unmap(sql)
	if args != nil {
		for i, a := range args {
			if s, ok := a.(string); ok {
				args[i] = Anonymizer.Unmap(s)
			}
		}
	}
	if err := RejectUnsafeSQL(sql); err != nil {
		return nil, err
	}
	// Full-literal alias/keyword filters become multi-variant comparisons so
	// they hit every stored spelling (Д-4/Д-5). After the precheck: the
	// expansion only rewrites operator+literal spans, never statements.
	sql = Anonymizer.ExpandAliasComparisons(sql, renderPGComparison)
	if args == nil {
		args = []any{}
	}

	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin read-only txn: %w", err)
	}
	defer tx.Rollback(ctx) // RO never commits; error is intentionally ignored

	// SET LOCAL does NOT support $1 parameter binding (PG limitation); use
	// set_config(name, value, is_local=true) instead — it accepts params, and
	// is_local=true is the SET LOCAL equivalent (scoped to this transaction).
	// The value is milliseconds as a plain integer: PG does NOT parse Go's
	// Duration.String() ("2m0s" is invalid — selftest catches this); "30s"
	// happens to parse but ms is unambiguous.
	if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)", strconv.FormatInt(StatementTimeout.Milliseconds(), 10)); err != nil {
		return nil, fmt.Errorf("set statement_timeout: %w", err)
	}

	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", wrapQueryError(err))
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	columns := make([]string, len(fds))
	oids := make([]uint32, len(fds))
	for i, fd := range fds {
		columns[i] = Anonymizer.Apply(fd.Name)
		oids[i] = fd.DataTypeOID
	}

	res := result.NewBuilder(columns)
	enc := newRowEncoder(oids)

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		encRow, chunk, err := enc.encode(values)
		if err != nil {
			return nil, err
		}
		if !res.TryAdd(encRow, chunk) {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", wrapQueryError(err))
	}

	// Notice assembly (metadata-before-rows keeps them readable when the
	// client chops oversized output). Truncation is prepended by Build;
	// the anonymization guard notices follow — they explain surprising-
	// looking results.
	var notices []string
	if enc.guardHits > 0 {
		notices = append(notices, "anonymization guard: some output values were redacted because they contained fragments of sensitive values (avoid functions over text columns that cut/transform values)")
	}
	if Anonymizer.HasPartialPattern(originalSQL) {
		notices = append(notices, "partial LIKE/ILIKE patterns are NOT translated to real values; matching may behave unexpectedly — prefer full-value filters (a pattern containing the whole alias works)")
	}
	if res.Emptyish() && Anonymizer.HasAliasEquality(originalSQL, originalArgs) {
		notices = append(notices, "equality ('=') filter on an anonymized alias returned 0 rows: the restored spelling may differ in case from the stored data — use ILIKE instead of '='")
	}
	out := res.Build(notices)
	// Notices carry counts (maxRows=4000, oversized sizes) and error-adjacent
	// wording — run the assembled text through Apply so a numeric rule or a
	// coincidental keyword can never surface verbatim.
	out.Notice = Anonymizer.Apply(out.Notice)
	return out, nil
}

// renderPGComparison renders the multi-variant comparison replacing an
// operator+literal span for ExpandAliasComparisons (PG dialect). Negated
// equality over a set is <> ALL (NOT = ANY would mean "differs from at least
// one" — wrong semantics).
func renderPGComparison(op string, negated bool, variants []string) string {
	quoted := make([]string, len(variants))
	for i, v := range variants {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	arr := "ARRAY[" + strings.Join(quoted, ",") + "]"
	switch op {
	case "=":
		if negated {
			return "<> ALL(" + arr + ")"
		}
		return "= ANY(" + arr + ")"
	default: // LIKE / ILIKE
		kw := op
		if negated {
			kw = "NOT " + op
		}
		return kw + " ANY (" + arr + ")"
	}
}

// NewPool builds a narrow pgxpool. MaxConns=1 per stdio process — tool calls
// in a session are serial, and a larger pool would starve other concurrent
// sessions sharing the role's CONNECTION LIMIT (design §3.4, §4.5).
func NewPool(dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	// statement_timeout is set per-query (SET LOCAL) inside RunReadOnly, NOT
	// globally here — see design §3.4.

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// truncationNotice explains WHY the output was cut, so the agent's next query
// is the right fix: a row that alone exceeds the whole byte budget needs a
// narrower projection, everything else needs aggregates/limits. (No
// length()/left()/substring() advice anymore — those functions are blocked by
// the anonymization guard, test report Д-1/Д-3.)
func truncationNotice(oversizedRow int) string {
	if oversizedRow > maxBytes {
		return fmt.Sprintf("output truncated: a single row (~%d bytes encoded) exceeds maxBytes=%d; project fewer/narrower columns instead of dumping wide values", oversizedRow, maxBytes)
	}
	return fmt.Sprintf("output truncated: server cap reached (maxRows=%d, maxBytes=%d). Prefer aggregates (GROUP BY / windows) over raw dumps of large fact tables.", maxRows, maxBytes)
}
