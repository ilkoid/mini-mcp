// Package main — MCP tool definitions for mcp-websearch.
//
// Two read-only tools, each carrying the mandatory annotation set
// {readOnlyHint:true, destructiveHint:false, idempotentHint:true}. The
// destructiveHint is a *bool specifically so an explicit "false" survives JSON
// marshalling (a plain bool:false with omitempty would be dropped) — this
// matters because the ZCode plan-mode predicate is `isMcp && !destructive`.
// (Same load-bearing detail as mcp-pg-readonly; see its tools.go.)
//
// The tool descriptions are the STEERING surface: they tell the agent these
// tools are free and local and must be preferred over hosted web-search /
// web-reader tools, which burn the coding plan's tool quota. Keep that
// wording when editing — a neutral description lets the model fall back to
// the built-in tools and the quota keeps draining.
package main

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ptr returns a pointer to v — used so *bool annotation fields with omitempty
// serialize their explicit value on the wire.
func ptr[T any](v T) *T { return &v }

// readOnlyAnnotations is the shared annotation block for both tools. Unlike
// mcp-pg-readonly (closed world: one database), these tools roam the open
// web, hence OpenWorldHint=true — still non-destructive either way.
var readOnlyAnnotations = &mcp.ToolAnnotations{
	ReadOnlyHint:    true,
	DestructiveHint: ptr(false), // explicit false on the wire (plan-mode !destructive)
	IdempotentHint:  true,
	OpenWorldHint:   ptr(true), // open world: arbitrary public URLs
}

// searchInput is the input for web_search (inputSchema is inferred).
type searchInput struct {
	Query      string `json:"query" jsonschema:"search query (required)"`
	Engine     string `json:"engine,omitempty" jsonschema:"search engine: ddg | bing | auto (default auto: ddg with bing fallback)"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"max results to return (default 10, max 20)"`
}

// readInput is the input for read_url.
type readInput struct {
	URL string `json:"url" jsonschema:"absolute http(s) URL to read (required)"`
}

const webSearchDescription = "FREE local web search (plain-HTTP scrape of DuckDuckGo/Bing from this machine — consumes NO plan quota, unlike hosted web-search tools). Make this your DEFAULT web-search tool instead of any built-in/metered search. Returns {query, engine, notice?, results:[{title,url,snippet}]} (default 10 hits). Follow up with read_url on a promising result. On a bot-wall notice (403/captcha), or when the target needs JS/login, switch to the interactive browser (browser-use) — not to metered hosted tools. Auto mode: ddg first, bing fallback; pass engine explicitly to pin one."

const readURLDescription = "FREE local page reader (plain-HTTP fetch + readable-text extraction — consumes NO plan quota, unlike hosted web-reader tools). Make this your DEFAULT for reading/opening a URL instead of any built-in/metered reader. Handles HTML and plain text, detects charsets (windows-1251 etc.). Returns {url, title, truncated, notice?, text} with text capped at 48KB. Not a JS browser: script-rendered pages (SPAs) yield little text — use the interactive browser (browser-use) for those, not metered hosted tools."

// registerTools wires the two tools onto the server.
func registerTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "web_search",
		Title:       "Web search (local, free)",
		Description: webSearchDescription,
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
		out, err := searchWeb(ctx, in.Query, in.Engine, in.MaxResults)
		if err != nil {
			// Surface tool errors inside Content with IsError=true (MCP spec),
			// not as protocol errors — so the LLM can self-correct.
			var r mcp.CallToolResult
			r.SetError(err)
			return &r, nil, nil
		}
		return resultJSON(out)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_url",
		Title:       "Read page (local, free)",
		Description: readURLDescription,
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
		out, err := readURL(ctx, in.URL)
		if err != nil {
			var r mcp.CallToolResult
			r.SetError(err)
			return &r, nil, nil
		}
		return resultJSON(out)
	})
}

// resultJSON marshals any envelope into a CallToolResult with a single text
// content block carrying the JSON.
func resultJSON(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		var r mcp.CallToolResult
		r.SetError(err)
		return &r, nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil, nil
}
