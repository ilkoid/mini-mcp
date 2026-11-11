// tools.go — MCP tool definitions for mcp-mssql-readonly.
//
// Four read-only tools, each carrying the mandatory annotation set
// {readOnlyHint:true, destructiveHint:false, idempotentHint:true}. The
// destructiveHint is a *bool specifically so an explicit "false" survives
// JSON marshalling (a plain bool:false with omitempty would be dropped) —
// this matters because the ZCode plan-mode predicate is `isMcp && !destructive`.
package mssql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ptr returns a pointer to v — used so destructiveHint/OpenWorldHint (*bool
// fields with omitempty) serialize their explicit value on the wire.
func ptr[T any](v T) *T { return &v }

// readOnlyAnnotations is the shared annotation block for every tool.
var readOnlyAnnotations = &mcp.ToolAnnotations{
	ReadOnlyHint:    true,
	DestructiveHint: ptr(false), // explicit false on the wire (plan-mode !destructive)
	IdempotentHint:  true,
	OpenWorldHint:   ptr(false), // closed world: just our SQL Server database
}

// queryInput is the input for mssql_readonly_query.
type queryInput struct {
	SQL  string        `json:"sql" jsonschema:"the T-SQL statement to execute. Prefer @p1,@p2 placeholders for values."`
	Args []interface{} `json:"args,omitempty" jsonschema:"bind values for @p1,@p2,... placeholders (positional)"`
}

// listTablesInput is the input for mssql_list_tables.
type listTablesInput struct {
	Schema  string `json:"schema,omitempty" jsonschema:"schema name (default: all schemas)"`
	Pattern string `json:"pattern,omitempty" jsonschema:"LIKE pattern for table names (default: %%)"`
}

// describeTableInput is the input for mssql_describe_table.
type describeTableInput struct {
	Table string `json:"table" jsonschema:"schema-qualified table name to describe (e.g. dbo.cards)"`
}

// queryDescriptionFmt is the mssql_readonly_query description. The role and
// the password-env name are interpolated from --user / --password-env so the
// sqlcmd-bypass hint always matches the role this entry actually runs as.
// Never hardcode a role/env name here — a wrong hint sends the agent after
// credentials that don't exist on that host.
const queryDescriptionFmt = "Read-only T-SQL against SQL Server (role %s). Returns JSON {columns, row_count, truncated, notice, rows}. Caps: 4000 rows / 48KB; for full result sets paginate with LIMIT/OFFSET and repeat. BIGINT and DECIMAL/NUMERIC/MONEY values arrive as JSON STRINGS to preserve precision (the driver delivers DECIMAL as text through float64 internally — CAST(col AS VARCHAR(64)) in SQL when you need the exact textual decimal; never cast IDs to float). Prefer aggregates (GROUP BY / windows) over raw dumps of large fact tables. Use @p1,@p2 placeholders for values (bind args positionally). Output is ANONYMIZED: sensitive keywords are replaced with aliases by the server (this is not configurable from your side); when filtering (WHERE/JOIN), use the ALIAS values you saw in previous results — they are translated back transparently; equality '=' is normally case-insensitive under CI collations, and full-literal equality/LIKE filters on an alias are auto-expanded to all stored spellings when configured. String-extraction, hash and length functions (LEFT/RIGHT/SUBSTRING/STUFF/REPLACE/CHARINDEX/HASHBYTES/COMPRESS/LEN/DATALENGTH/…) and CAST/CONVERT to VARBINARY are BLOCKED by the anonymization guard — they can leak derivatives of sensitive values; select the column itself or use aggregates instead. Partial LIKE patterns (like '%%frag%%') are NOT translated and may behave unexpectedly — prefer full-value filters (a pattern containing the whole alias works). The same visible alias may legitimately appear as two GROUP BY rows (two data eras of stored values). For heavier read-only analytics (beyond these caps or near the timeout), run sqlcmd directly with the SAME read-only role (%s, password from env %s — NEVER the admin password); the role is SELECT-only, so this stays safe."

// toolErr builds the standard error result. Error text is masked: DB error
// messages happily echo values (constraint names, conversion errors) and
// must not leak keywords into the session.
func toolErr(err error) (*mcp.CallToolResult, any, error) {
	var r mcp.CallToolResult
	r.SetError(Anonymizer.ApplyErr(err))
	return &r, nil, nil
}

// RegisterTools wires the four read-only tools onto the server. role/pwdEnv
// feed the description (see queryDescriptionFmt).
func RegisterTools(s *mcp.Server, db *sql.DB, role, pwdEnv string) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mssql_readonly_query",
		Title:       "Read-only T-SQL query",
		Description: fmt.Sprintf(queryDescriptionFmt, role, role, pwdEnv),
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, any, error) {
		args := make([]any, len(in.Args))
		for i, a := range in.Args {
			args[i] = a
		}
		res, err := RunReadOnly(ctx, db, in.SQL, args)
		if err != nil {
			// Surface tool errors inside Content with IsError=true (MCP
			// spec), not as a protocol error — so the LLM can self-correct.
			return toolErr(err)
		}
		return resultJSON(res)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mssql_list_dbs",
		Title:       "List databases",
		Description: "List databases visible on this SQL Server instance. Returns {columns, rows, row_count, truncated, notice} with one row per database: name, state_desc, collation_name.",
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		const q = `SELECT name, state_desc, collation_name FROM sys.databases ORDER BY name`
		res, err := RunReadOnly(ctx, db, q, nil)
		if err != nil {
			return toolErr(err)
		}
		return resultJSON(res)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mssql_list_tables",
		Title:       "List tables",
		Description: "List tables and views in the current database (sys.objects; empty schema = all schemas). Returns {columns, rows, row_count, truncated, notice} with one row per object: schema_name, name, type (BASE TABLE / VIEW).",
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listTablesInput) (*mcp.CallToolResult, any, error) {
		schema := in.Schema
		pattern := in.Pattern
		if pattern == "" {
			pattern = "%"
		}
		const q = `SELECT s.name AS schema_name, o.name AS name,
		              CASE o.type WHEN 'U' THEN N'BASE TABLE' WHEN 'V' THEN N'VIEW' ELSE o.type_desc END AS type
		           FROM sys.objects o
		           JOIN sys.schemas s ON o.schema_id = s.schema_id
		           WHERE o.type IN ('U','V') AND o.name LIKE @p1 AND (@p2 = N'' OR s.name = @p2)
		           ORDER BY s.name, o.name`
		res, err := RunReadOnly(ctx, db, q, []any{pattern, schema})
		if err != nil {
			return toolErr(err)
		}
		return resultJSON(res)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mssql_describe_table",
		Title:       "Describe table",
		Description: "Describe a table: columns (name, type, nullable, default), primary/unique constraints, and indexes (key columns). Input is a schema-qualified name (e.g. dbo.cards). Returns an object {table, columns, constraints, indexes}.",
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in describeTableInput) (*mcp.CallToolResult, any, error) {
		if in.Table == "" {
			return toolErr(fmt.Errorf("table is required"))
		}
		out, err := DescribeTable(ctx, db, in.Table)
		if err != nil {
			return toolErr(err)
		}
		b, err := json.Marshal(out)
		if err != nil {
			return toolErr(err)
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

// describeOutput is the structured shape of mssql_describe_table's JSON.
type describeOutput struct {
	Table       string        `json:"table"`
	Columns     []tableColumn `json:"columns"`
	Constraints []tableConstr `json:"constraints,omitempty"`
	Indexes     []tableIndex  `json:"indexes,omitempty"`
}

type tableColumn struct {
	Name          string `json:"name"`
	DataType      string `json:"data_type"`
	IsNullable    bool   `json:"is_nullable"`
	ColumnDefault string `json:"column_default,omitempty"`
}

type tableConstr struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"` // PRIMARY KEY, UNIQUE, ...
	Columns []string `json:"columns"`
}

type tableIndex struct {
	Name    string   `json:"name"`
	Unique  bool     `json:"unique"`
	Type    string   `json:"type"` // CLUSTERED / NONCLUSTERED / ...
	Columns []string `json:"columns"`
}

// DescribeTable runs the two catalog queries backing mssql_describe_table.
// Each runs through RunReadOnly so masking applies to names/defaults too.
func DescribeTable(ctx context.Context, db *sql.DB, table string) (*describeOutput, error) {
	out := &describeOutput{Table: Anonymizer.Apply(table)}

	colRes, err := RunReadOnly(ctx, db, `
		SELECT c.name AS column_name, t.name AS type_name,
		       CASE WHEN c.is_nullable = 1 THEN 1 ELSE 0 END AS is_nullable,
		       OBJECT_DEFINITION(c.default_object_id) AS column_default
		FROM sys.columns c
		JOIN sys.types t ON t.user_type_id = c.user_type_id
		WHERE c.object_id = OBJECT_ID(@p1)
		ORDER BY c.column_id`, []any{table})
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	if colRes.RowCount == 0 {
		// SQL Server tables always have >=1 column; zero rows here means
		// the table doesn't exist (or isn't visible to the role).
		return nil, fmt.Errorf("table %q not found (use mssql_list_tables to discover tables)", table)
	}
	for _, row := range colRes.Rows {
		c := tableColumn{}
		if v, ok := cellString(row, 0); ok {
			c.Name = v
		}
		if v, ok := cellString(row, 1); ok {
			c.DataType = v
		}
		if len(row) > 2 {
			if n, ok := row[2].(int64); ok {
				c.IsNullable = n != 0
			}
		}
		if v, ok := cellString(row, 3); ok {
			c.ColumnDefault = v
		}
		out.Columns = append(out.Columns, c)
	}

	conRes, err := RunReadOnly(ctx, db, `
		SELECT kc.name AS constraint_name, kc.type_desc AS constraint_type,
		       c2.name AS column_name
		FROM sys.key_constraints kc
		JOIN sys.index_columns ic ON ic.object_id = kc.parent_object_id AND ic.index_id = kc.unique_index_id
		JOIN sys.columns c2 ON c2.object_id = ic.object_id AND c2.column_id = ic.column_id
		WHERE kc.parent_object_id = OBJECT_ID(@p1)
		ORDER BY kc.name, ic.key_ordinal`, []any{table})
	if err != nil {
		return nil, fmt.Errorf("constraints: %w", err)
	}
	var cur *tableConstr
	for _, row := range conRes.Rows {
		name, _ := cellString(row, 0)
		typ, _ := cellString(row, 1)
		col, _ := cellString(row, 2)
		if cur == nil || cur.Name != name {
			out.Constraints = append(out.Constraints, tableConstr{Name: name, Type: typ})
			cur = &out.Constraints[len(out.Constraints)-1]
		}
		cur.Columns = append(cur.Columns, col)
	}

	// Indexes (key columns, ordered): the tester's live finding 2026-09-04
	// (report №4) — plans for 2.6-billion-row heaps need the index list to
	// reason about seeks. Row-per-(index,ordinal), grouped here — no
	// STRING_AGG, so commas in column names can't corrupt the split.
	idxRes, err := RunReadOnly(ctx, db, `
		SELECT i.name AS index_name,
		       CASE WHEN i.is_unique = 1 THEN 1 ELSE 0 END AS is_unique,
		       i.type_desc AS type_desc,
		       c.name AS column_name
		FROM sys.indexes i
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE i.object_id = OBJECT_ID(@p1) AND i.index_id > 0 AND ic.is_included_column = 0
		ORDER BY i.index_id, ic.key_ordinal`, []any{table})
	if err != nil {
		return nil, fmt.Errorf("indexes: %w", err)
	}
	var curIdx *tableIndex
	for _, row := range idxRes.Rows {
		name, _ := cellString(row, 0)
		unique := false
		if len(row) > 1 {
			if n, ok := row[1].(int64); ok {
				unique = n != 0
			}
		}
		typ, _ := cellString(row, 2)
		col, _ := cellString(row, 3)
		if curIdx == nil || curIdx.Name != name {
			out.Indexes = append(out.Indexes, tableIndex{Name: name, Unique: unique, Type: typ})
			curIdx = &out.Indexes[len(out.Indexes)-1]
		}
		curIdx.Columns = append(curIdx.Columns, col)
	}

	return out, nil
}

// cellString extracts a string cell from an encoded row (plain string or a
// JSON string RawMessage).
func cellString(row []interface{}, i int) (string, bool) {
	if len(row) <= i {
		return "", false
	}
	switch v := row[i].(type) {
	case string:
		return v, true
	case json.RawMessage:
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return s, true
		}
	}
	return "", false
}
