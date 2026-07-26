// Package main — read-only query core for mcp-pg-readonly.
//
// This file is the security boundary's runtime side (the database role is the
// primary boundary — see role-bootstrap.sql). runReadOnly wraps every agent
// SQL in a READ ONLY transaction with statement_timeout and output caps, and
// encodes results so that BIGINT/numeric survive the JSON float64 round-trip
// (WB nm_id values exceed 2^53). See design doc §4.2–4.7.
package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Result is the JSON shape returned to the agent. The {columns, rows,
// row_count, truncated, notice} envelope is fixed so the agent always knows
// whether output was cut, even though the ZCode client also applies its own
// resultBudget truncation (design §4.3 — double truncation is intentional;
// server caps stay ≤ client budget).
type Result struct {
	Columns   []string        `json:"columns"`
	Rows      [][]interface{} `json:"rows"`
	RowCount  int             `json:"row_count"`
	Truncated bool            `json:"truncated"`
	Notice    string          `json:"notice,omitempty"`
}

// Output caps (design §4.3).
const (
	maxRows  = 500
	maxBytes = 64 * 1024 // server-side soft cap on the JSON payload
)

// Per-query safety knobs (design §4.2).
const (
	queryTimeout    = 60 * time.Second // ctx deadline wrapping the whole call
	statementTimeout = 30 * time.Second // SET LOCAL statement_timeout inside the RO txn
)

// nonReadKeywords are rejected up-front as DX (NOT a security boundary — the
// role + READ ONLY txn are the boundary). Matching the leading keyword keeps
// the agent from getting a confusing role-denied error for an obvious typo.
var nonReadKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "TRUNCATE", "MERGE",
	"ALTER", "DROP", "CREATE", "GRANT", "REVOKE",
	"COPY", "CALL", "DO", "VACUUM", "REINDEX", "ANALYZE",
	"CLUSTER", "REFRESH", "COMMENT",
}

// trailingSemicolonRe matches a single trailing ';' possibly followed by space.
var trailingSemicolonRe = regexp.MustCompile(`;\s*$`)

// rejectUnsafeSQL enforces the single-statement policy (design §4.4):
//   - reject empty / whitespace-only;
//   - reject more than one statement (any ';' before the optional trailing one);
//   - reject leading non-read keywords as a DX precheck.
//
// This is intentionally NOT the security boundary — a malicious multi-statement
// payload that slipped past would still be stopped by the READ ONLY transaction
// and the mcp_ro role ACLs.
func rejectUnsafeSQL(sql string) error {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return errors.New("empty SQL")
	}

	// Single statement: strip ONE optional trailing ';', then any remaining ';'
	// means multiple statements (or a ';' inside the body, which we also reject
	// to keep the rule simple — agents should use $1..$n params, not inline).
	body := trailingSemicolonRe.ReplaceAllString(trimmed, "")
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

// runReadOnly executes a single SQL statement in a READ ONLY transaction with
// a 30s statement_timeout, always rolls back, and applies row/byte caps.
func runReadOnly(ctx context.Context, pool *pgxpool.Pool, sql string, args []any) (*Result, error) {
	if err := rejectUnsafeSQL(sql); err != nil {
		return nil, err
	}
	if args == nil {
		args = []any{}
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin read-only txn: %w", err)
	}
	defer tx.Rollback(ctx) // RO never commits; error is intentionally ignored

	// SET LOCAL does NOT support $1 parameter binding (PG limitation); use
	// set_config(name, value, is_local=true) instead — it accepts params, and
	// is_local=true is the SET LOCAL equivalent (scoped to this transaction).
	if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)", statementTimeout.String()); err != nil {
		return nil, fmt.Errorf("set statement_timeout: %w", err)
	}

	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	columns := make([]string, len(fds))
	oids := make([]uint32, len(fds))
	for i, fd := range fds {
		columns[i] = fd.Name
		oids[i] = fd.DataTypeOID
	}

	res := &Result{Columns: columns, Rows: [][]interface{}{}}
	enc := newRowEncoder(oids)
	var byteBudget = maxBytes

	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		encRow, chunk, err := enc.encode(values)
		if err != nil {
			return nil, err
		}
		if chunk > byteBudget {
			res.Truncated = true
			break
		}
		byteBudget -= chunk
		res.Rows = append(res.Rows, encRow)
		res.RowCount++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	if res.Truncated {
		res.Notice = fmt.Sprintf("output truncated: server cap reached (maxRows=%d, maxBytes=%d). Prefer aggregates (GROUP BY / windows) over raw dumps of large fact tables.", maxRows, maxBytes)
	}
	return res, nil
}
