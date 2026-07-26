module github.com/ilkoid/mcp-pg-readonly

// mcp-pg-readonly — a tiny read-only PostgreSQL MCP server for the ZCode agent.
//
// Separate Go module (NOT part of poncho-ai). Sources live in ~/dev/mini-mcp,
// binary installs to ~/bin/mcp-pg-readonly — see README.md. The module/binary
// name follows the design doc's canon (mcp-pg-readonly) for a stable user-scope
// MCP `command` path that survives repo renames.

go 1.25.0

require (
	github.com/jackc/pgx/v5 v5.9.2
	github.com/modelcontextprotocol/go-sdk v1.4.0
)

require (
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.3 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.34.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
