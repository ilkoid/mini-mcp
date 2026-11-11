// Package result — the shared tool-result envelope for the read-only MCP
// servers (mcp-pg-readonly, mcp-mssql-readonly). Dialect packages encode
// their rows however the driver hands them over, then feed the encoded rows
// into a Builder which enforces the row/byte caps and assembles the final
// JSON shape. The envelope contract itself (metadata before rows, caps below
// the client budget) is the load-bearing part — see design §4.3.
package result

import (
	"fmt"
	"strings"
)

// Result is the JSON shape returned to the agent. Field order is deliberate:
// metadata (row_count, truncated, notice) precedes rows because the ZCode
// client truncates oversized tool output by chopping the JSON tail — this way
// the agent still sees that output was cut even when the rows are mangled
// (design §4.3 — double truncation is intentional; server caps stay ≤ client
// budget).
type Result struct {
	Columns   []string        `json:"columns"`
	RowCount  int             `json:"row_count"`
	Truncated bool            `json:"truncated"`
	Notice    string          `json:"notice,omitempty"`
	Rows      [][]interface{} `json:"rows"`
}

// Output caps (design §4.3). MaxBytes must stay BELOW the ZCode client's
// resultBudget (~50KB observed) including envelope overhead, or every
// byte-capped result gets its tail (incl. notice) chopped by the client —
// that budget is the hard ceiling a single tool response cannot exceed, so
// "more data in one call" beyond ~48KB is not achievable by the server.
// MaxRows is the secondary guard; 500 → 2000 (2026-09-04, live analytics
// chunking pain) → 4000 (owner decision the same day). For narrow rows the
// byte cap still binds first, so the raise only cuts round-trips without
// enlarging the worst-case payload.
const (
	MaxRows  = 4000
	MaxBytes = 48 * 1024
)

// Builder accumulates encoded rows under the caps. The caller encodes a row
// (dialect-specific masking included), then calls TryAdd with the row and its
// approximate JSON byte cost; TryAdd records truncation itself and returns
// false when iteration must stop. Build assembles the envelope, prepending
// the truncation notice (if any) to the dialect-supplied notices.
type Builder struct {
	res       *Result
	budget    int
	oversized int // >0 when truncation dropped a row that alone exceeds MaxBytes
}

// NewBuilder starts a result for the given (already masked) column names.
func NewBuilder(columns []string) *Builder {
	return &Builder{
		res:    &Result{Columns: columns, Rows: [][]interface{}{}},
		budget: MaxBytes,
	}
}

// TryAdd appends an encoded row of chunk JSON bytes. ok=false means capacity
// is exhausted (row cap reached, or the row alone exceeds the byte budget) —
// stop iterating; Truncated is already set on the eventual Result.
func (b *Builder) TryAdd(row []interface{}, chunk int) bool {
	if b.res.RowCount >= MaxRows {
		b.res.Truncated = true
		return false
	}
	if chunk > b.budget {
		b.res.Truncated = true
		b.oversized = chunk
		return false
	}
	b.budget -= chunk
	b.res.Rows = append(b.res.Rows, row)
	b.res.RowCount++
	return true
}

// Emptyish reports the two shapes a matched-nothing filter takes: zero rows
// at all, or the classic count(*) probe returning a single zero (row_count
// is 1 there — checking only RowCount missed it, live finding 2026-09-03).
// Used by the dialect layers for the equality-on-alias hint notice.
func (b *Builder) Emptyish() bool {
	if b.res.RowCount == 0 {
		return true
	}
	if b.res.RowCount != 1 || len(b.res.Rows) != 1 {
		return false
	}
	for _, cell := range b.res.Rows[0] {
		switch v := cell.(type) {
		case string:
			if v != "0" {
				return false
			}
		case int:
			if v != 0 {
				return false
			}
		case int64:
			if v != 0 {
				return false
			}
		case float64:
			if v != 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Build finalizes the Result. The truncation notice is PREPENDED to the
// dialect notices (truncation explains the shape; the anonymization-guard
// notices explain surprising-looking content — both must survive the
// client's tail-chop, hence metadata-before-rows).
func (b *Builder) Build(notices []string) *Result {
	if b.res.Truncated {
		notices = append([]string{TruncationNotice(b.oversized)}, notices...)
	}
	if len(notices) > 0 {
		b.res.Notice = strings.Join(notices, " | ")
	}
	return b.res
}

// TruncationNotice explains WHY the output was cut, so the agent's next query
// is the right fix: a row that alone exceeds the whole byte budget needs a
// narrower projection, everything else needs aggregates/limits. (No
// length()/left()/substring() advice — those functions are blocked by the
// anonymization guard, test report Д-1/Д-3.)
func TruncationNotice(oversizedRow int) string {
	if oversizedRow > MaxBytes {
		return fmt.Sprintf("output truncated: a single row (~%d bytes encoded) exceeds maxBytes=%d; project fewer/narrower columns instead of dumping wide values", oversizedRow, MaxBytes)
	}
	return fmt.Sprintf("output truncated: server cap reached (maxRows=%d, maxBytes=%d). Prefer aggregates (GROUP BY / windows) over raw dumps of large fact tables; to fetch a full result set, paginate with LIMIT/OFFSET (or keyset on an ordered column) and repeat the call.", MaxRows, MaxBytes)
}
