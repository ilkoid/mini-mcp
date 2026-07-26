// Package main — MCP tool definitions for mcp-pg-readonly.
//
// Three read-only tools (design §5), each carrying the mandatory annotation
// set {readOnlyHint:true, destructiveHint:false, idempotentHint:true}. The
// destructiveHint is a *bool specifically so an explicit "false" survives
// JSON marshalling (a plain bool:false with omitempty would be dropped) — this
// matters because the ZCode plan-mode predicate is `isMcp && !destructive`.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ptr returns a pointer to v — used so destructiveHint/OpenWorldHint (*bool
// fields with omitempty) serialize their explicit value on the wire.
func ptr[T any](v T) *T { return &v }

// readOnlyAnnotations is the shared annotation block for every tool. See §5.0.
var readOnlyAnnotations = &mcp.ToolAnnotations{
	ReadOnlyHint:    true,
	DestructiveHint: ptr(false), // explicit false on the wire (plan-mode !destructive)
	IdempotentHint:  true,
	OpenWorldHint:   ptr(false), // closed world: just our PG database
}

// queryInput is the input for pg_readonly_query (inputSchema is inferred).
type queryInput struct {
	SQL  string        `json:"sql" jsonschema:"the SQL statement to execute. Prefer $1,$2 placeholders for values."`
	Args []interface{} `json:"args,omitempty" jsonschema:"bind values for $1,$2,... placeholders"`
}

// listTablesInput is the input for pg_list_tables.
type listTablesInput struct {
	Schema  string `json:"schema,omitempty" jsonschema:"schema name (default: public)"`
	Pattern string `json:"pattern,omitempty" jsonschema:"LIKE pattern for table names (default: %)"`
}

// describeTableInput is the input for pg_describe_table.
type describeTableInput struct {
	Schema string `json:"schema,omitempty" jsonschema:"schema name (default: public)"`
	Table  string `json:"table" jsonschema:"table name to describe"`
}

// registerTools wires the three read-only tools onto the server.
func registerTools(s *mcp.Server, pool *pgxpool.Pool) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "pg_readonly_query",
		Title:       "Read-only SQL query",
		Description: "Read-only SQL against PostgreSQL (role mcp_ro, READ ONLY transaction). Returns JSON {columns, rows, row_count, truncated, notice}. Caps: 500 rows / 64KB. Prefer aggregates (GROUP BY / windows) over raw dumps of large fact tables (e.g. sales). Use $1,$2 placeholders for values.",
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, any, error) {
		args := make([]any, len(in.Args))
		for i, a := range in.Args {
			args[i] = a
		}
		res, err := runReadOnly(ctx, pool, in.SQL, args)
		if err != nil {
			// Surface tool errors inside Content with IsError=true (MCP spec),
			// not as a protocol error — so the LLM can self-correct.
			var r mcp.CallToolResult
			r.SetError(err)
			return &r, nil, nil
		}
		return resultJSON(res)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "pg_list_tables",
		Title:       "List tables",
		Description: "List tables in a PostgreSQL schema (defaults to public). Returns {columns, rows, row_count, truncated, notice} with one row per table: schema, name, type (BASE TABLE / VIEW / ...).",
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listTablesInput) (*mcp.CallToolResult, any, error) {
		schema := in.Schema
		if schema == "" {
			schema = "public"
		}
		pattern := in.Pattern
		if pattern == "" {
			pattern = "%"
		}
		const sql = `SELECT table_schema AS schema, table_name AS name, table_type AS type
		             FROM information_schema.tables
		             WHERE table_schema = $1 AND table_name LIKE $2
		             ORDER BY table_name`
		res, err := runReadOnly(ctx, pool, sql, []any{schema, pattern})
		if err != nil {
			var r mcp.CallToolResult
			r.SetError(err)
			return &r, nil, nil
		}
		return resultJSON(res)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "pg_describe_table",
		Title:       "Describe table",
		Description: "Describe a PostgreSQL table: columns (name, type, nullable, default), primary/unique constraints, and indexes. Returns an object {schema, table, columns, constraints, indexes}.",
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in describeTableInput) (*mcp.CallToolResult, any, error) {
		schema := in.Schema
		if schema == "" {
			schema = "public"
		}
		if in.Table == "" {
			var r mcp.CallToolResult
			r.SetError(fmt.Errorf("table is required"))
			return &r, nil, nil
		}
		out, err := describeTable(ctx, pool, schema, in.Table)
		if err != nil {
			var r mcp.CallToolResult
			r.SetError(err)
			return &r, nil, nil
		}
		b, err := json.Marshal(out)
		if err != nil {
			var r mcp.CallToolResult
			r.SetError(err)
			return &r, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, nil, nil
	})
}

// resultJSON marshals a *Result into a CallToolResult with a single text
// content block carrying the JSON envelope.
func resultJSON(res *Result) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(res)
	if err != nil {
		var r mcp.CallToolResult
		r.SetError(err)
		return &r, nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil, nil
}

// describeOutput is the structured shape of pg_describe_table's JSON.
type describeOutput struct {
	Schema      string          `json:"schema"`
	Table       string          `json:"table"`
	Columns     []tableColumn   `json:"columns"`
	Constraints []tableConstr   `json:"constraints,omitempty"`
	Indexes     []tableIndex    `json:"indexes,omitempty"`
}

type tableColumn struct {
	Name        string `json:"name"`
	DataType    string `json:"data_type"`
	IsNullable  bool   `json:"is_nullable"`
	ColumnDefault string `json:"column_default,omitempty"`
}

type tableConstr struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"` // PRIMARY KEY, UNIQUE, ...
	Columns []string `json:"columns"`
}

type tableIndex struct {
	Name   string `json:"name"`
	Unique bool   `json:"unique"`
	Def    string `json:"definition"`
}

// describeTable runs the three small catalog queries that back
// pg_describe_table. Each runs in its own read-only transaction (they're
// independent), so a failure in one still leaves the others readable.
func describeTable(ctx context.Context, pool *pgxpool.Pool, schema, table string) (*describeOutput, error) {
	out := &describeOutput{Schema: schema, Table: table}

	colRes, err := runReadOnly(ctx, pool, `
		SELECT column_name, data_type,
		       (is_nullable = 'YES'),
		       column_default::text
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, []any{schema, table})
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	for _, row := range colRes.Rows {
		c := tableColumn{}
		if len(row) > 0 {
			if s, ok := row[0].(string); ok {
				c.Name = s
			} else if rm, ok := row[0].(json.RawMessage); ok {
				_ = json.Unmarshal(rm, &c.Name)
			}
		}
		if len(row) > 1 {
			if s, ok := row[1].(string); ok {
				c.DataType = s
			} else if rm, ok := row[1].(json.RawMessage); ok {
				_ = json.Unmarshal(rm, &c.DataType)
			}
		}
		if len(row) > 2 {
			if b, ok := row[2].(bool); ok {
				c.IsNullable = b
			}
		}
		if len(row) > 3 {
			if s, ok := row[3].(string); ok {
				c.ColumnDefault = s
			} else if rm, ok := row[3].(json.RawMessage); ok {
				_ = json.Unmarshal(rm, &c.ColumnDefault)
			}
		}
		out.Columns = append(out.Columns, c)
	}

	conRes, err := runReadOnly(ctx, pool, `
		SELECT tc.constraint_name, tc.constraint_type,
		       string_agg(kcu.column_name, ',' ORDER BY kcu.ordinal_position)
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema = kcu.table_schema
		WHERE tc.table_schema = $1 AND tc.table_name = $2
		  AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')
		GROUP BY tc.constraint_name, tc.constraint_type`, []any{schema, table})
	if err != nil {
		return nil, fmt.Errorf("constraints: %w", err)
	}
	for _, row := range conRes.Rows {
		var c tableConstr
		if len(row) > 0 {
			if s, ok := row[0].(string); ok {
				c.Name = s
			} else if rm, ok := row[0].(json.RawMessage); ok {
				_ = json.Unmarshal(rm, &c.Name)
			}
		}
		if len(row) > 1 {
			if s, ok := row[1].(string); ok {
				c.Type = s
			} else if rm, ok := row[1].(json.RawMessage); ok {
				_ = json.Unmarshal(rm, &c.Type)
			}
		}
		if len(row) > 2 {
			if s, ok := row[2].(string); ok {
				c.Columns = splitCSV(s)
			} else if rm, ok := row[2].(json.RawMessage); ok {
				var s string
				_ = json.Unmarshal(rm, &s)
				c.Columns = splitCSV(s)
			}
		}
		out.Constraints = append(out.Constraints, c)
	}

	idxRes, err := runReadOnly(ctx, pool, `
		SELECT indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = $1 AND tablename = $2
		ORDER BY indexname`, []any{schema, table})
	if err != nil {
		return nil, fmt.Errorf("indexes: %w", err)
	}
	for _, row := range idxRes.Rows {
		var idx tableIndex
		if len(row) > 0 {
			if s, ok := row[0].(string); ok {
				idx.Name = s
			} else if rm, ok := row[0].(json.RawMessage); ok {
				_ = json.Unmarshal(rm, &idx.Name)
			}
		}
		if len(row) > 1 {
			if s, ok := row[1].(string); ok {
				idx.Def = s
				idx.Unique = containsCI(s, "UNIQUE")
			} else if rm, ok := row[1].(json.RawMessage); ok {
				var s string
				_ = json.Unmarshal(rm, &s)
				idx.Def = s
				idx.Unique = containsCI(s, "UNIQUE")
			}
		}
		out.Indexes = append(out.Indexes, idx)
	}

	return out, nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range splitOnComma(s) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitOnComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	out = append(out, cur)
	return out
}

func containsCI(s, sub string) bool {
	return indexOfCI(s, sub) >= 0
}

func indexOfCI(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	ls := toLowerASCII(s)
	lsub := toLowerASCII(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return i
		}
	}
	return -1
}

func toLowerASCII(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
