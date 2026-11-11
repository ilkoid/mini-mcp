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
	"errors"
	"fmt"
	"strings"

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

	mcp.AddTool(s, &mcp.Tool{
		Name:        "bridge_status",
		Title:       "Level-4 bridge diagnostics",
		Description: bridgeStatusDescription,
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in bridgeStatusInput) (*mcp.CallToolResult, any, error) {
		info := bridged.statusInfo()
		if errS, ok := info["error"]; ok {
			var r mcp.CallToolResult
			r.SetError(fmt.Errorf("%v (спустись по лестнице: read_url / интерактивный браузер)", errS))
			return &r, nil, nil
		}
		return resultJSON(info)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "profile_fetch",
		Title:       "Fetch via real browser profile (level 4)",
		Description: profileFetchDescription,
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in profileFetchInput) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.URL) == "" {
			var r mcp.CallToolResult
			r.SetError(errors.New("url is required"))
			return &r, nil, nil
		}
		res, err := bridged.dispatch(&Job{ID: newJobID(), Action: actionFetch, URL: in.URL, Raw: in.Raw})
		if err != nil {
			return bridgeErrResult(err)
		}
		return resultJSON(shapeProfileFetch(res, in.Raw))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "profile_tab",
		Title:       "Open tab in real browser (level 4)",
		Description: profileTabDescription,
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in profileTabInput) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.URL) == "" {
			var r mcp.CallToolResult
			r.SetError(errors.New("url is required"))
			return &r, nil, nil
		}
		res, err := bridged.dispatch(&Job{ID: newJobID(), Action: actionTabRead, URL: in.URL, WaitMs: in.WaitMs})
		if err != nil {
			return bridgeErrResult(err)
		}
		return resultJSON(shapeProfileTab(res))
	})
}

// bridgeStatusInput — bridge_status takes no arguments (kept explicit for
// schema clarity).
type bridgeStatusInput struct{}

// profileFetchInput is the input for profile_fetch.
type profileFetchInput struct {
	URL string `json:"url" jsonschema:"absolute http(s) URL (required)"`
	Raw bool   `json:"raw,omitempty" jsonschema:"return raw HTML (capped at 48KB) instead of extracted readable text"`
}

// profileTabInput is the input for profile_tab.
type profileTabInput struct {
	URL    string `json:"url" jsonschema:"absolute http(s) URL (required)"`
	WaitMs int    `json:"wait_ms,omitempty" jsonschema:"extra settle time after load in ms (default 3000, capped at 15000) — lets JS and soft challenges resolve"`
}

// profileFetchOutput is the profile_fetch envelope (metadata BEFORE text,
// same ordering discipline as ReadOutput).
type profileFetchOutput struct {
	Status    int    `json:"status"`
	FinalURL  string `json:"final_url"`
	Truncated bool   `json:"truncated"`
	Notice    string `json:"notice,omitempty"`
	Title     string `json:"title,omitempty"`
	Text      string `json:"text"`
}

// profileTabOutput is the profile_tab envelope.
type profileTabOutput struct {
	FinalURL  string `json:"final_url"`
	Title     string `json:"title,omitempty"`
	Truncated bool   `json:"truncated"`
	Notice    string `json:"notice,omitempty"`
	Text      string `json:"text"`
}

// shapeProfileFetch turns a bridge Result into the tool payload: readable
// text via the same extractHTML as read_url (unless raw), capped at 48KB.
// Non-2xx statuses are still RESULTS (the caller decides what a 403/498
// body means — e.g. the WB stub); only transport errors are errors.
func shapeProfileFetch(res *Result, raw bool) *profileFetchOutput {
	out := &profileFetchOutput{Status: res.Status, FinalURL: res.FinalURL}
	body := res.Body
	if raw {
		out.Text, out.Truncated = capText(body)
		if out.Truncated {
			out.Notice = fmt.Sprintf("raw HTML capped at %dKB", maxTextBytes>>10)
		}
		return out
	}
	text, title := extractHTML([]byte(body))
	out.Title = title
	out.Text, out.Truncated = capText(strings.TrimSpace(text))
	return out
}

// shapeProfileTab turns a tab_read Result into the tool payload (innerText
// is already text; just cap it).
func shapeProfileTab(res *Result) *profileTabOutput {
	out := &profileTabOutput{FinalURL: res.FinalURL, Title: res.Title}
	out.Text, out.Truncated = capText(strings.TrimSpace(res.Body))
	return out
}

// bridgeErrResult maps bridge-level failures onto MCP results: busy is NOT
// an error (a retryable payload with retry_in_ms), everything else is a
// tool error steering down the ladder.
func bridgeErrResult(err error) (*mcp.CallToolResult, any, error) {
	var busy *busyError
	if errors.As(err, &busy) {
		return resultJSON(map[string]any{
			"busy":        true,
			"retry_in_ms": busy.retryMs,
			"notice":      "мост занят другим заданием — повтори вызов через момент",
		})
	}
	var r mcp.CallToolResult
	r.SetError(fmt.Errorf("%v (спустись по лестнице: read_url → интерактивный браузер; мост обычно не нужен)", err))
	return &r, nil, nil
}

const bridgeStatusDescription = "Level-4 diagnostics: the real-browser bridge (own MV3 extension + local mailbox hub inside mcp-websearch, 127.0.0.1:8765). Reports role (owner/client), hub address, token file, and whether the extension is currently polling (online/offline). Call this FIRST when profile_fetch/profile_tab misbehave and follow its hints. Levels: 1 web_search → 2 read_url → 3 interactive browser → 4 this bridge; level 4 is ONLY for sites that block 1-3 (WB-class anti-bot)."

const profileFetchDescription = "Level 4 — fetch a URL from the user's REAL browser profile (their Edge/Chrome via the local bridge extension): full cookies/logins, real-browser fingerprint, but NO page rendering. Use ONLY for sites that block levels 1-3 (read_url / interactive browser): WB-class anti-bot walls. Returns {status, final_url, truncated, title, text} (text ≤48KB; raw=true → raw HTML). Etiquette: the server enforces ≥2s between jobs; on a bot-wall/interstitial response do NOT retry the same URL — report it as blocked."

const profileTabDescription = "Level 4 — open a URL in a visible pinned tab of the user's real browser, let JS/challenges settle (wait_ms, default 3000, max 15000), return {final_url, title, text} (innerText ≤48KB). The most honest signal vs anti-bots (real navigation + real JS, like a user). One tab is reused across calls (session cookies persist). If an anti-bot interstitial appears — do NOT wait out any timer and do NOT retry: report and stop (WB's timer is a known trap)."

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
