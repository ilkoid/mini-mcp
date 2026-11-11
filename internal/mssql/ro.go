// ro.go — the execution path: Unmap on the way in, precheck, two-era filter
// expansion, context timeouts, caps and guards on the way out.
package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"mini-mcp/internal/anon"
	"mini-mcp/internal/result"
)

// Anonymizer is the process-wide keyword-masking engine (shared rules file
// with the PG sibling). nil = pass-through (unit tests default);
// cmd/mcp-mssql-readonly requires --anon-config and sets this before serving.
var Anonymizer *anon.Anonymizer

// Result is the shared JSON envelope (internal/result).
type Result = result.Result

// Per-query safety knobs. Unlike PG there is no server-side statement_timeout
// reachable through the driver — both layers are client-side context
// deadlines: the statement ctx cancels the in-flight query first (the driver
// sends the TDS attention packet), the wider call ctx covers scan/cleanup.
// Defaults mirror the PG sibling: 2min statement < 3min call.
var (
	QueryTimeout     = 180 * time.Second // ctx deadline wrapping the whole call
	StatementTimeout = 120 * time.Second // per-statement ctx deadline
)

// renderTSQLComparison renders the multi-variant comparison replacing an
// operator+literal span for ExpandAliasComparisons (T-SQL dialect). There is
// no = ANY / LIKE ANY in T-SQL; IN (…) covers it, and a full-literal LIKE
// (no wildcards) is semantically equal to equality under both CI and CS
// collations, so both map to IN. Negated forms map to NOT IN. The rendered
// text carries a LEADING SPACE: the replaced span starts at the operator, so
// the left operand can be glued to it ("brand=N'x'" must not become
// "brandIN").
func renderTSQLComparison(op string, negated bool, variants []string) string {
	quoted := make([]string, len(variants))
	for i, v := range variants {
		quoted[i] = "N'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	if negated {
		return " NOT IN (" + strings.Join(quoted, ",") + ")"
	}
	return " IN (" + strings.Join(quoted, ",") + ")"
}

// RunReadOnly executes a single T-SQL statement under the caps and applies
// the bidirectional anonymization choke point: Unmap on the way in (agent
// aliases become real values before the database sees them, then
// full-literal alias filters expand to all stored spellings via eq_any —
// two-era data), Apply on the way out (column names, notices) — cell values
// are masked and fragment-guarded by the row encoder.
func RunReadOnly(ctx context.Context, db *sql.DB, sql string, args []any) (*Result, error) {
	// Pre-Unmap snapshot: the zero-rows and partial-pattern notices must
	// judge the SQL the AGENT wrote (aliases), not the restored keywords.
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
	// Full-literal alias/keyword filters become multi-variant IN (…) so they
	// hit every stored spelling (Д-4/Д-5). After the precheck: the expansion
	// only rewrites operator+literal spans, never statements.
	sql = Anonymizer.ExpandAliasComparisons(sql, renderTSQLComparison)
	if args == nil {
		args = []any{}
	}

	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
	stmtCtx, stmtCancel := context.WithTimeout(ctx, StatementTimeout)
	defer stmtCancel()

	rows, err := db.QueryContext(stmtCtx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", wrapQueryError(err))
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", wrapQueryError(err))
	}
	for i := range columns {
		columns[i] = Anonymizer.Apply(columns[i])
	}

	colTypes, _ := rows.ColumnTypes()
	enc := newRowEncoder(dbTypeNames(colTypes, len(columns)))

	b := result.NewBuilder(columns)
	for rows.Next() {
		dests := enc.scanTargets()
		if err := rows.Scan(dests...); err != nil {
			return nil, fmt.Errorf("scan row: %w", wrapQueryError(err))
		}
		encRow, chunk, err := enc.encode(dests)
		if err != nil {
			return nil, err
		}
		if !b.TryAdd(encRow, chunk) {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", wrapQueryError(err))
	}

	// Notice assembly (metadata-before-rows keeps them readable when the
	// client chops oversized output). Truncation is prepended by Build.
	var notices []string
	if enc.guardHits > 0 {
		notices = append(notices, "anonymization guard: some output values were redacted because they contained fragments of sensitive values (avoid functions over text columns that cut/transform values)")
	}
	if Anonymizer.HasPartialPattern(originalSQL) {
		notices = append(notices, "partial LIKE patterns are NOT translated to real values; matching may behave unexpectedly — prefer full-value filters (a pattern containing the whole alias works)")
	}
	if b.Emptyish() && Anonymizer.HasAliasEquality(originalSQL, originalArgs) {
		notices = append(notices, "equality ('=') filter on an anonymized alias returned 0 rows: the restored spelling may differ from the stored data (stored-spelling variants are operator-configured)")
	}
	out := b.Build(notices)
	// Notices carry counts and error-adjacent wording — run the assembled
	// text through Apply so a numeric rule or a coincidental keyword can
	// never surface verbatim.
	out.Notice = Anonymizer.Apply(out.Notice)
	return out, nil
}

// dbTypeNames extracts DatabaseTypeName per column, tolerant of drivers or
// code paths that return fewer/no column types (value-driven fallback then
// kicks in inside the encoder).
func dbTypeNames(cts []*sql.ColumnType, n int) []string {
	names := make([]string, n)
	for i := 0; i < n && i < len(cts); i++ {
		names[i] = cts[i].DatabaseTypeName()
	}
	return names
}

// wrapQueryError translates cancellation failures into actionable messages.
// Both timeout layers are client-side contexts; the driver surfaces them as
// context.DeadlineExceeded (or an mssql "Operation cancelled" style error).
func wrapQueryError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("query exceeded timeout (statement %s, whole call %s); add filters/limits/aggregates or raise --statement-timeout: %w", StatementTimeout, QueryTimeout, err)
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "canceled") {
		return fmt.Errorf("query was cancelled (likely timeout: statement %s); add filters/limits/aggregates or raise --statement-timeout: %w", StatementTimeout, err)
	}
	return err
}
