module github.com/ilkoid/mcp-websearch

// mcp-websearch — a tiny quota-free web search / page reader MCP server.
//
// Second independent module in the mini-mcp lab (sibling of mcp-pg-readonly,
// same stdio idioms). Scrapes plain-HTML SERPs (DuckDuckGo/Bing) and fetches
// pages over plain HTTP — everything stays local, so calls consume none of the
// hosted-tool quota of the coding plan. Binary installs to ~/bin/mcp-websearch.

go 1.25.0

require (
	github.com/PuerkitoBio/goquery v1.10.0
	github.com/modelcontextprotocol/go-sdk v1.4.0
	golang.org/x/net v0.29.0
	golang.org/x/text v0.18.0
)

require (
	github.com/andybalholm/cascadia v1.3.2 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.3 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.34.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
)
