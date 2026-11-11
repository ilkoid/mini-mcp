module mini-mcp

// mcp-pg-readonly — a tiny read-only PostgreSQL MCP server for the ZCode agent.
//
// Separate Go module (NOT part of poncho-ai). Sources live in ~/dev/mini-mcp,
// binary installs to ~/bin/mcp-pg-readonly — see README.md. The module/binary
// name follows the design doc's canon (mcp-pg-readonly) for a stable user-scope
// MCP `command` path that survives repo renames.

go 1.25.0

require (
	github.com/jackc/pgx/v5 v5.9.2
	github.com/microsoft/go-mssqldb v1.11.0
	github.com/modelcontextprotocol/go-sdk v1.4.0
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.59.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.3 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/oauth2 v0.34.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
